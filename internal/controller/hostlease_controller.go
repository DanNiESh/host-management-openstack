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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/host-management-openstack/internal/helpers"
	"github.com/osac-project/host-management-openstack/internal/ironic"
	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

const hostClass = "openstack"

var recheckInterval = helpers.GetEnvWithDefault(
	"HOST_RECHECK_INTERVAL",
	60*time.Second,
	func(d time.Duration) bool { return d > 0 },
)

// HostLeaseReconciler reconciles HostLease CRs for power management via Ironic.
type HostLeaseReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	IronicClient *ironic.Client
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases/status,verbs=get;update;patch
func (r *HostLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	hostLease := &v1alpha1.HostLease{}
	if err := r.Get(ctx, req.NamespacedName, hostLease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hostLease.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Check if the HostLease should be managed by this controller
	if !r.validateOpenStackHost(hostLease, log) {
		return ctrl.Result{}, nil
	}

	node, err := r.IronicClient.GetNode(ctx, hostLease.Spec.ExternalID)
	if err != nil {
		log.Error(err, "failed to get Ironic node", "nodeID", hostLease.Spec.ExternalID)
		return ctrl.Result{}, err
	}
	log.V(1).Info("Ironic node", "nodeID", hostLease.Spec.ExternalID, "power_state", node.PowerState)

	// Skip power reconciliation while Ironic is processing a provisioning transition.
	if !ironic.IsProvisioning(node) {
		if err := r.reconcilePower(ctx, hostLease, node, log); err != nil {
			if statusErr := r.Status().Update(ctx, hostLease); statusErr != nil {
				log.Error(statusErr, "failed to update HostLease status after power failure")
			}
			return ctrl.Result{}, err
		}
	}

	// Reconcile provisioning state if spec.provisioning is set.
	if res, err := r.reconcileProvisioning(ctx, hostLease, node, log); err != nil || !res.IsZero() {
		if err != nil {
			if statusErr := r.Status().Update(ctx, hostLease); statusErr != nil {
				log.Error(statusErr, "failed to update HostLease status after provisioning failure")
			}
		}
		return res, err
	}

	// Refresh node state after reconciliation.
	node, err = r.IronicClient.GetNode(ctx, hostLease.Spec.ExternalID)
	if err != nil {
		log.Error(err, "failed to refresh node after reconciliation")
		return ctrl.Result{}, err
	}

	// Sync the HostLease status
	r.syncHostLeaseStatus(hostLease, node)

	// Update the HostLease status
	if err := r.Status().Update(ctx, hostLease); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to poll Ironic if power or provisioning state doesn't match desired.
	if r.needsRequeue(hostLease, node) {
		return ctrl.Result{RequeueAfter: recheckInterval}, nil
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
	currentlyOn := node.PowerState == ironic.PowerOn.String()
	var err error

	if hostLease.Spec.PoweredOn && !currentlyOn {
		log.Info("Powering on node", "nodeID", hostLease.Spec.ExternalID)
		if err = r.IronicClient.SetPowerState(ctx, hostLease.Spec.ExternalID, ironic.PowerOn); err != nil {
			log.Error(err, "failed to power on node", "nodeID", hostLease.Spec.ExternalID)
			hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionFalse,
				v1alpha1.HostConditionReasonIronicAPIFailure, fmt.Sprintf("failed to power on node: %v", err))
		}
	} else if !hostLease.Spec.PoweredOn && currentlyOn {
		log.Info("Powering off node", "nodeID", hostLease.Spec.ExternalID)
		if err = r.IronicClient.SetPowerState(ctx, hostLease.Spec.ExternalID, ironic.PowerOff); err != nil {
			log.Error(err, "failed to power off node", "nodeID", hostLease.Spec.ExternalID)
			hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionFalse,
				v1alpha1.HostConditionReasonIronicAPIFailure, fmt.Sprintf("failed to power off node: %v", err))
		}
	} else {
		log.V(1).Info("Power state already matches desired", "poweredOn", hostLease.Spec.PoweredOn, "power_state", node.PowerState)
	}

	return err
}

func (r *HostLeaseReconciler) reconcileProvisioning(ctx context.Context, hostLease *v1alpha1.HostLease, node *nodes.Node, log logr.Logger) (ctrl.Result, error) {
	if hostLease.Spec.Provisioning == nil {
		return ctrl.Result{}, nil
	}

	desiredState := hostLease.Spec.Provisioning.ProvisioningState

	switch desiredState {
	case v1alpha1.ProvisioningStateActive:
		return r.reconcileDesiredActive(ctx, hostLease, node, log)
	case v1alpha1.ProvisioningStateAvailable:
		return r.reconcileDesiredAvailable(ctx, hostLease, node, log)
	default:
		log.V(1).Info("HostLease skipped", "reason", "spec.provisioning.provisioningState not active or available", "state", desiredState)
		return ctrl.Result{}, nil
	}
}

func (r *HostLeaseReconciler) reconcileDesiredActive(ctx context.Context, hostLease *v1alpha1.HostLease, node *nodes.Node, log logr.Logger) (ctrl.Result, error) {
	isoURL := strings.TrimSpace(hostLease.Spec.Provisioning.ImageSpec.URL)
	if isoURL == "" || strings.TrimSpace(hostLease.Spec.Provisioning.ProvisioningNetwork) == "" {
		return ctrl.Result{}, nil
	}

	if ironic.IsProvisioning(node) {
		log.Info("Node is deploying, waiting", "provision_state", node.ProvisionState)
		return ctrl.Result{RequeueAfter: recheckInterval}, nil
	}

	if ironic.IsProvisioned(node) {
		log.V(1).Info("Node is already deployed")
		return ctrl.Result{}, nil
	}

	log.Info("Provisioning node with ISO", "nodeID", hostLease.Spec.ExternalID, "isoURL", isoURL)
	if err := r.IronicClient.ProvisionWithISO(ctx, hostLease.Spec.ExternalID, isoURL); err != nil {
		switch {
		case errors.Is(err, ironic.ErrNodeNotAvailable):
			log.Info("Provision deferred", "reason", err.Error())
			return ctrl.Result{RequeueAfter: recheckInterval}, nil
		case errors.Is(err, ironic.ErrValidationFailed):
			log.Error(err, "Ironic validation failed before deploy")
			hostLease.SetStatusCondition(v1alpha1.HostConditionProvisioned, metav1.ConditionFalse,
				v1alpha1.HostConditionReasonIronicAPIFailure, fmt.Sprintf("validation failed: %v", err))
			return ctrl.Result{}, err
		default:
			log.Error(err, "ProvisionWithISO failed")
			hostLease.SetStatusCondition(v1alpha1.HostConditionProvisioned, metav1.ConditionFalse,
				v1alpha1.HostConditionReasonIronicAPIFailure, fmt.Sprintf("provisioning failed: %v", err))
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: recheckInterval}, nil
}

func (r *HostLeaseReconciler) reconcileDesiredAvailable(ctx context.Context, hostLease *v1alpha1.HostLease, node *nodes.Node, log logr.Logger) (ctrl.Result, error) {
	ps := nodes.ProvisionState(node.ProvisionState)

	if ps == nodes.Available {
		log.V(1).Info("Node is already available")
		return ctrl.Result{}, nil
	}

	if ps == nodes.Manageable {
		log.Info("Moving node from manageable to available", "nodeID", hostLease.Spec.ExternalID)
		if err := r.IronicClient.ChangeProvisionState(ctx, hostLease.Spec.ExternalID, nodes.ProvisionStateOpts{Target: nodes.TargetProvide}); err != nil {
			log.Error(err, "failed to move node to available")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: recheckInterval}, nil
	}

	if ironic.CanDeprovision(node) {
		log.Info("Deprovisioning node", "nodeID", hostLease.Spec.ExternalID, "provision_state", node.ProvisionState)
		if err := r.IronicClient.ChangeProvisionState(ctx, hostLease.Spec.ExternalID, nodes.ProvisionStateOpts{Target: nodes.TargetDeleted}); err != nil {
			log.Error(err, "failed to deprovision node")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: recheckInterval}, nil
	}

	if ironic.IsProvisioning(node) {
		log.Info("Node is transitioning, waiting", "provision_state", node.ProvisionState)
		return ctrl.Result{RequeueAfter: recheckInterval}, nil
	}

	return ctrl.Result{}, nil
}

// syncHostLeaseStatus syncs the HostLease status from Ironic node state.
func (r *HostLeaseReconciler) syncHostLeaseStatus(hostLease *v1alpha1.HostLease, node *nodes.Node) {
	poweredOn := node.PowerState == ironic.PowerOn.String()
	hostLease.Status.PoweredOn = &poweredOn

	if poweredOn {
		hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionTrue,
			v1alpha1.HostConditionReasonPowerOn, "")
	} else {
		hostLease.SetStatusCondition(v1alpha1.HostConditionPowerSynced, metav1.ConditionTrue,
			v1alpha1.HostConditionReasonPowerOff, "")
	}

	// Sync provisioning status
	hostLease.Status.Provisioning.ProvisioningState = node.ProvisionState
	if hostLease.Spec.Provisioning != nil && hostLease.Spec.Provisioning.ProvisioningState == v1alpha1.ProvisioningStateActive {
		hostLease.Status.Provisioning.URL = hostLease.Spec.Provisioning.ImageSpec.URL
	}

	if ironic.IsProvisioned(node) {
		hostLease.SetStatusCondition(v1alpha1.HostConditionProvisioned, metav1.ConditionTrue, "Provisioned", "")
	} else if ironic.IsDeployFailed(node) {
		hostLease.SetStatusCondition(v1alpha1.HostConditionProvisioned, metav1.ConditionFalse, "DeployFailed", node.LastError)
	}
}

func (r *HostLeaseReconciler) needsRequeue(hostLease *v1alpha1.HostLease, node *nodes.Node) bool {
	// Requeue if power state doesn't match desired.
	currentlyOn := node.PowerState == ironic.PowerOn.String()
	if hostLease.Spec.PoweredOn != currentlyOn {
		return true
	}

	// Requeue if provisioning is in progress.
	if ironic.IsProvisioning(node) {
		return true
	}

	// Requeue if provisioning state doesn't match desired.
	if hostLease.Spec.Provisioning != nil {
		ps := nodes.ProvisionState(node.ProvisionState)
		switch hostLease.Spec.Provisioning.ProvisioningState {
		case v1alpha1.ProvisioningStateActive:
			if ps != nodes.Active {
				return true
			}
		case v1alpha1.ProvisioningStateAvailable:
			if ps != nodes.Available {
				return true
			}
		}
	}

	return false
}

func (r *HostLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HostLease{}).
		Named("openstack-host").
		Complete(r)
}
