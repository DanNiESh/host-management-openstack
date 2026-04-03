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
	"time"

	"fmt"

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

	if err := r.reconcilePower(ctx, hostLease, node, log); err != nil {
		if statusErr := r.Status().Update(ctx, hostLease); statusErr != nil {
			log.Error(statusErr, "failed to update HostLease status after power failure")
		}
		return ctrl.Result{}, err
	}

	node, err = r.IronicClient.GetNode(ctx, hostLease.Spec.ExternalID)
	if err != nil {
		log.Error(err, "failed to refresh node after power reconciliation")
		return ctrl.Result{}, err
	}

	// Sync the HostLease status
	r.syncHostLeaseStatus(hostLease, node)

	// Update the HostLease status
	if err := r.Status().Update(ctx, hostLease); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to poll Ironic until power state matches desired; stop once synced.
	currentlyOn := node.PowerState == ironic.PowerOn.String()
	if hostLease.Spec.PoweredOn != currentlyOn {
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

// Sync the HostLease status and update corresponding conditions
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
}

func (r *HostLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HostLease{}).
		Named("openstack-host").
		Complete(r)
}
