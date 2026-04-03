/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller implements Kubernetes controllers for managing OpenStack resources.
package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/osac-project/bare-metal-operator/api/v1alpha1"
	"github.com/osac-project/host-management-openstack/internal/ironic"
)

// HostLeaseReconciler reconciles HostLease CRs for power management via Ironic.
type HostLeaseReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	IronicClient ironic.NodeClient

	// RecheckInterval is the interval for polling Ironic until power state matches desired state.
	RecheckInterval time.Duration
}

// NewHostLeaseReconciler creates a new HostLeaseReconciler with defaults applied.
func NewHostLeaseReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	ironicClient ironic.NodeClient,
	recheckInterval time.Duration,
) *HostLeaseReconciler {
	if recheckInterval <= 0 {
		recheckInterval = DefaultRecheckInterval
	}

	return &HostLeaseReconciler{
		Client:          client,
		Scheme:          scheme,
		IronicClient:    ironicClient,
		RecheckInterval: recheckInterval,
	}
}

// Reconcile manages the lifecycle of HostLease resources by reconciling their power state with Ironic.
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases/finalizers,verbs=update;patch
func (r *HostLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	hostLease := &v1alpha1.HostLease{}
	if err := r.Get(ctx, req.NamespacedName, hostLease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hostLease.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(hostLease, hostLeaseFinalizer) {
			log.V(1).Info("Skipping cleanup: finalizer not present", "finalizer", hostLeaseFinalizer)
			return ctrl.Result{}, nil
		}

		log.Info("Running HostLease cleanup", "finalizer", hostLeaseFinalizer)

		// Only clean up HostLeases managed by openstack.
		if !r.validateOpenStackHost(hostLease, log) {
			log.Info("Skipping cleanup: HostLease not managed by this controller")
			return ctrl.Result{}, nil
		}
		hostLease.Status.Phase = v1alpha1.HostLeasePhaseDeleting
		if err := r.Status().Update(ctx, hostLease); err != nil {
			log.Error(err, "failed to update HostLease status phase to Deleting")
			return ctrl.Result{}, err
		}

		// Remove the finalizer and unset the hostClass
		log.Info("Unsetting hostClass and removing finalizer")
		hostLease.Spec.HostClass = ""
		controllerutil.RemoveFinalizer(hostLease, hostLeaseFinalizer)
		if err := r.Update(ctx, hostLease); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Cleanup complete, finalizer removed")

		return ctrl.Result{}, nil
	}

	// Check if the HostLease should be managed by this controller
	if !r.validateOpenStackHost(hostLease, log) {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(hostLease, hostLeaseFinalizer) {
		controllerutil.AddFinalizer(hostLease, hostLeaseFinalizer)
		if err := r.Update(ctx, hostLease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	node, err := r.IronicClient.GetNode(ctx, hostLease.Spec.ExternalID)
	if err != nil {
		log.Error(err, "failed to get Ironic node", "nodeID", hostLease.Spec.ExternalID)
		return ctrl.Result{}, err
	}
	log.V(1).Info("Ironic node", "nodeID", hostLease.Spec.ExternalID, "power_state", node.PowerState)

	// Return before power reconciliation and status sync.
	if hostLease.Spec.PoweredOn == nil {
		return ctrl.Result{}, nil
	}

	if err := r.reconcilePower(ctx, hostLease, node, log); err != nil {
		r.syncHostLeaseStatus(hostLease, nil, err)
		if statusErr := r.Status().Update(ctx, hostLease); statusErr != nil {
			log.Error(statusErr, "failed to update HostLease status after power failure")
		}
		return ctrl.Result{}, err
	}

	node, err = r.IronicClient.GetNode(ctx, hostLease.Spec.ExternalID)
	if err != nil {
		log.Error(err, "failed to refresh node after power reconciliation", "nodeID", hostLease.Spec.ExternalID, "Error", err.Error())
		r.syncHostLeaseStatus(hostLease, nil, err)
		if statusErr := r.Status().Update(ctx, hostLease); statusErr != nil {
			log.Error(statusErr, "failed to update HostLease status after node refresh failure")
		}
		return ctrl.Result{}, err
	}

	// Sync the HostLease status
	r.syncHostLeaseStatus(hostLease, node, nil)

	// Update the HostLease status
	if err := r.Status().Update(ctx, hostLease); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to poll Ironic until power state matches desired; stop once synced.
	currentlyOn := node.PowerState == ironic.PowerOn.String()
	if *hostLease.Spec.PoweredOn != currentlyOn {
		return ctrl.Result{RequeueAfter: r.RecheckInterval}, nil
	}

	return ctrl.Result{}, nil
}

func (r *HostLeaseReconciler) validateOpenStackHost(hostLease *v1alpha1.HostLease, log logr.Logger) bool {
	if hostLease.Spec.ExternalID == "" {
		log.Info("HostLease skipped", "reason", "spec.externalID not set")
		return false
	}

	if hostLease.Spec.HostClass != hostClass {
		log.V(1).Info("HostLease skipped", "reason", "hostClass mismatch", "want", hostClass, "got", hostLease.Spec.HostClass)
		return false
	}

	return true
}

func (r *HostLeaseReconciler) reconcilePower(ctx context.Context, hostLease *v1alpha1.HostLease, node *nodes.Node, log logr.Logger) error {
	if hostLease.Spec.PoweredOn == nil {
		log.V(1).Info("PoweredOn is nil, skipping power reconciliation", "nodeID", hostLease.Spec.ExternalID)
		return nil
	}

	currentlyOn := node.PowerState == ironic.PowerOn.String()
	desiredOn := *hostLease.Spec.PoweredOn

	// If Ironic is already processing a power state change, skip to avoid 409 Conflict.
	if r.IronicClient.IsNodePowerTransitioning(node) {
		log.V(1).Info("Node is transitioning, skipping power action",
			"nodeID", hostLease.Spec.ExternalID,
			"targetPowerState", node.TargetPowerState)
		return nil
	}

	var err error
	if desiredOn && !currentlyOn {
		log.Info("Powering on node", "nodeID", hostLease.Spec.ExternalID)
		if err = r.IronicClient.SetPowerState(ctx, hostLease.Spec.ExternalID, ironic.PowerOn); err != nil {
			log.Error(err, "failed to power on node", "nodeID", hostLease.Spec.ExternalID)
		}
	} else if !desiredOn && currentlyOn {
		log.Info("Powering off node", "nodeID", hostLease.Spec.ExternalID)
		if err = r.IronicClient.SetPowerState(ctx, hostLease.Spec.ExternalID, ironic.PowerOff); err != nil {
			log.Error(err, "failed to power off node", "nodeID", hostLease.Spec.ExternalID)
		}
	} else {
		log.V(1).Info("Power state already matches desired", "poweredOn", desiredOn, "power_state", node.PowerState)
	}

	return err
}

// syncHostLeaseStatus syncs the phase, conditions, and power state.
func (r *HostLeaseReconciler) syncHostLeaseStatus(hostLease *v1alpha1.HostLease, node *nodes.Node, reconcileErr error) {
	// If there is an error during power reconciliation, set the status condition to false
	if reconcileErr != nil {
		hostLease.Status.Phase = v1alpha1.HostLeasePhaseFailed
		hostLease.SetStatusCondition(
			v1alpha1.HostConditionPowerSynced,
			metav1.ConditionFalse,
			v1alpha1.HostConditionReasonIronicAPIFailure,
			reconcileErr.Error(),
		)
		return
	}

	if node == nil {
		return
	}

	poweredOn := node.PowerState == ironic.PowerOn.String()
	hostLease.Status.PoweredOn = &poweredOn

	// Set phase based on whether actual power state matches desired.
	if hostLease.Spec.PoweredOn != nil && *hostLease.Spec.PoweredOn != poweredOn {
		hostLease.Status.Phase = v1alpha1.HostLeasePhaseProgressing
	} else {
		hostLease.Status.Phase = v1alpha1.HostLeasePhaseReady
	}

	if poweredOn {
		hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionTrue,
			v1alpha1.HostConditionReasonPowerOn, "")
		return
	}

	hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionTrue,
		v1alpha1.HostConditionReasonPowerOff, "")
}

func (r *HostLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HostLease{}).
		Named("openstack-host").
		Complete(r)
}
