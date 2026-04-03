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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1alpha1 "github.com/osac-project/bare-metal-operator/api/v1alpha1"
	"github.com/osac-project/host-management-openstack/internal/ironic"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// mockIronicClient implements ironic.NodeClient for testing.
type mockIronicClient struct {
	getNodeFunc                  func(ctx context.Context, nodeID string) (*nodes.Node, error)
	setPowerStateFunc            func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error
	isNodePowerTransitioningFunc func(node *nodes.Node) bool
}

func (m *mockIronicClient) GetNode(ctx context.Context, nodeID string) (*nodes.Node, error) {
	if m.getNodeFunc != nil {
		return m.getNodeFunc(ctx, nodeID)
	}
	return &nodes.Node{PowerState: "power off"}, nil
}

func (m *mockIronicClient) SetPowerState(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
	if m.setPowerStateFunc != nil {
		return m.setPowerStateFunc(ctx, nodeID, target)
	}
	return nil
}

func (m *mockIronicClient) IsNodePowerTransitioning(node *nodes.Node) bool {
	if m.isNodePowerTransitioningFunc != nil {
		return m.isNodePowerTransitioningFunc(node)
	}
	return node.TargetPowerState != ""
}

func boolPtr(b bool) *bool {
	return &b
}

var _ = Describe("HostLeaseReconciler", func() {
	var (
		reconciler *HostLeaseReconciler
		mockIronic *mockIronicClient
		testScheme *runtime.Scheme
		log        = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
	)

	BeforeEach(func() {
		logf.SetLogger(log)
		testScheme = runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(testScheme)).To(Succeed())

		mockIronic = &mockIronicClient{}
		reconciler = &HostLeaseReconciler{
			Scheme:          testScheme,
			IronicClient:    mockIronic,
			RecheckInterval: 10 * time.Second,
		}
	})

	Describe("NewHostLeaseReconciler", func() {
		It("should use the provided recheck interval when positive", func() {
			r := NewHostLeaseReconciler(nil, testScheme, mockIronic, 30*time.Second)
			Expect(r.RecheckInterval).To(Equal(30 * time.Second))
		})

		It("should use the default recheck interval when zero", func() {
			r := NewHostLeaseReconciler(nil, testScheme, mockIronic, 0)
			Expect(r.RecheckInterval).To(Equal(DefaultRecheckInterval))
		})
	})

	Describe("validateOpenStackHost", func() {
		It("should return false when ExternalID is empty", func() {
			hostLease := &v1alpha1.HostLease{
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "",
					HostClass:  "openstack",
				},
			}
			Expect(reconciler.validateOpenStackHost(hostLease, log)).To(BeFalse())
		})

		It("should return false when HostClass does not match", func() {
			hostLease := &v1alpha1.HostLease{
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-1",
					HostClass:  "other",
				},
			}
			Expect(reconciler.validateOpenStackHost(hostLease, log)).To(BeFalse())
		})

		It("should return true when ExternalID and HostClass are valid", func() {
			hostLease := &v1alpha1.HostLease{
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-1",
					HostClass:  "openstack",
				},
			}
			Expect(reconciler.validateOpenStackHost(hostLease, log)).To(BeTrue())
		})
	})

	Describe("reconcilePower", func() {
		var (
			ctx       context.Context
			hostLease *v1alpha1.HostLease
		)

		BeforeEach(func() {
			ctx = context.Background()
			hostLease = &v1alpha1.HostLease{
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-1",
					HostClass:  "openstack",
				},
			}
		})

		It("should power on when desired on and currently off", func() {
			hostLease.Spec.PoweredOn = boolPtr(true)
			node := &nodes.Node{PowerState: "power off"}

			var calledTarget ironic.TargetPowerState
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				calledTarget = target
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(calledTarget.String()).To(Equal(ironic.PowerOn.String()))
		})

		It("should power off when desired off and currently on", func() {
			hostLease.Spec.PoweredOn = boolPtr(false)
			node := &nodes.Node{PowerState: "power on"}

			var calledTarget ironic.TargetPowerState
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				calledTarget = target
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(calledTarget.String()).To(Equal(ironic.PowerOff.String()))
		})

		It("should not call SetPowerState when power state already matches (on)", func() {
			hostLease.Spec.PoweredOn = boolPtr(true)
			node := &nodes.Node{PowerState: "power on"}

			called := false
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				called = true
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(called).To(BeFalse())
		})

		It("should not call SetPowerState when power state already matches (off)", func() {
			hostLease.Spec.PoweredOn = boolPtr(false)
			node := &nodes.Node{PowerState: "power off"}

			called := false
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				called = true
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(called).To(BeFalse())
		})

		It("should skip power reconciliation when PoweredOn is nil", func() {
			hostLease.Spec.PoweredOn = nil
			node := &nodes.Node{PowerState: "power on"}

			called := false
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				called = true
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(called).To(BeFalse())
		})

		It("should skip SetPowerState when node is transitioning", func() {
			hostLease.Spec.PoweredOn = boolPtr(true)
			node := &nodes.Node{PowerState: "power off", TargetPowerState: "power on"}

			called := false
			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				called = true
				return nil
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(called).To(BeFalse())
		})

		It("should return error when SetPowerState fails on power on", func() {
			hostLease.Spec.PoweredOn = boolPtr(true)
			node := &nodes.Node{PowerState: "power off"}

			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				return errors.New("ironic API error")
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ironic API error"))
		})

		It("should return error when SetPowerState fails on power off", func() {
			hostLease.Spec.PoweredOn = boolPtr(false)
			node := &nodes.Node{PowerState: "power on"}

			mockIronic.setPowerStateFunc = func(ctx context.Context, nodeID string, target ironic.TargetPowerState) error {
				return errors.New("ironic API error")
			}

			err := reconciler.reconcilePower(ctx, hostLease, node, log)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ironic API error"))
		})
	})

	Describe("Reconcile", func() {
		It("should add finalizer for managed host lease", func() {
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-add-finalizer",
					Namespace: "default",
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-finalizer",
					HostClass:  hostClass,
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(hostLease).
				Build()

			getNodeCalls := 0
			mockIronic.getNodeFunc = func(_ context.Context, _ string) (*nodes.Node, error) {
				getNodeCalls++
				return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(getNodeCalls).To(Equal(0))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updatedHostLease, hostLeaseFinalizer)).To(BeTrue())
		})

		It("should unset host class and remove finalizer on delete", func() {
			now := metav1.Now()
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "hostlease-delete",
					Namespace:         "default",
					Finalizers:        []string{hostLeaseFinalizer},
					DeletionTimestamp: &now,
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-delete",
					HostClass:  hostClass,
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(hostLease).
				WithObjects(hostLease).
				Build()

			getNodeCalls := 0
			setPowerStateCalls := 0
			mockIronic.getNodeFunc = func(_ context.Context, _ string) (*nodes.Node, error) {
				getNodeCalls++
				return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
			}
			mockIronic.setPowerStateFunc = func(_ context.Context, _ string, _ ironic.TargetPowerState) error {
				setPowerStateCalls++
				return nil
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(getNodeCalls).To(Equal(0))
			Expect(setPowerStateCalls).To(Equal(0))

			// Fake client deletes the object once all finalizers are removed
			// and DeletionTimestamp is set, so verify it no longer exists.
			updatedHostLease := &v1alpha1.HostLease{}
			err = reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())
		})

		It("should not clean up non-openstack hostClass on delete", func() {
			now := metav1.Now()
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "hostlease-delete-non-openstack",
					Namespace:         "default",
					Finalizers:        []string{hostLeaseFinalizer},
					DeletionTimestamp: &now,
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-delete-non-openstack",
					HostClass:  "other-provider",
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(hostLease).
				Build()

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Spec.HostClass).To(Equal("other-provider"))
			Expect(controllerutil.ContainsFinalizer(updatedHostLease, hostLeaseFinalizer)).To(BeTrue())
		})

		It("should not clean up openstack hostClass with empty externalID on delete", func() {
			now := metav1.Now()
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "hostlease-delete-openstack-no-externalid",
					Namespace:         "default",
					Finalizers:        []string{hostLeaseFinalizer},
					DeletionTimestamp: &now,
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "",
					HostClass:  hostClass,
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(hostLease).
				Build()

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Spec.HostClass).To(Equal(hostClass))
			Expect(controllerutil.ContainsFinalizer(updatedHostLease, hostLeaseFinalizer)).To(BeTrue())
		})

		It("should skip power reconcile and status sync when PoweredOn is nil", func() {
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-sample",
					Namespace: "default",
					Finalizers: []string{
						hostLeaseFinalizer,
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-1",
					HostClass:  hostClass,
					PoweredOn:  nil,
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(hostLease).
				WithObjects(hostLease).
				Build()

			getNodeCalls := 0
			setPowerStateCalls := 0
			mockIronic.getNodeFunc = func(_ context.Context, _ string) (*nodes.Node, error) {
				getNodeCalls++
				return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
			}
			mockIronic.setPowerStateFunc = func(_ context.Context, _ string, _ ironic.TargetPowerState) error {
				setPowerStateCalls++
				return nil
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(getNodeCalls).To(Equal(1))
			Expect(setPowerStateCalls).To(Equal(0))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Status.PoweredOn).To(BeNil())
			Expect(updatedHostLease.Status.Conditions).To(BeEmpty())
		})

		It("should update status when PoweredOn is set", func() {
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-managed",
					Namespace: "default",
					Finalizers: []string{
						hostLeaseFinalizer,
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-2",
					HostClass:  hostClass,
					PoweredOn:  boolPtr(false),
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(hostLease).
				WithObjects(hostLease).
				Build()

			mockIronic.getNodeFunc = func(_ context.Context, _ string) (*nodes.Node, error) {
				return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
			}
			mockIronic.setPowerStateFunc = func(_ context.Context, _ string, _ ironic.TargetPowerState) error {
				Fail("SetPowerState should not be called when power already matches desired")
				return nil
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseReady))
			Expect(updatedHostLease.Status.PoweredOn).NotTo(BeNil())
			Expect(*updatedHostLease.Status.PoweredOn).To(BeFalse())
			condition := updatedHostLease.GetStatusCondition(v1alpha1.HostConditionPowerSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(v1alpha1.HostConditionReasonPowerOff))
		})

		It("should requeue when power is not yet converged", func() {
			requeueInterval := 7 * time.Second
			reconciler.RecheckInterval = requeueInterval

			desiredOn := true
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-requeue",
					Namespace: "default",
					Finalizers: []string{
						hostLeaseFinalizer,
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-requeue",
					HostClass:  hostClass,
					PoweredOn:  &desiredOn,
				},
			}
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(hostLease).
				WithObjects(hostLease).
				Build()

			getNodeCalls := 0
			mockIronic.getNodeFunc = func(_ context.Context, _ string) (*nodes.Node, error) {
				getNodeCalls++
				if getNodeCalls == 1 {
					return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
				}
				return &nodes.Node{PowerState: ironic.PowerOff.String()}, nil
			}
			mockIronic.setPowerStateFunc = func(_ context.Context, _ string, _ ironic.TargetPowerState) error {
				return nil
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueInterval))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseProgressing))
		})
	})

	Describe("syncHostLeaseStatus", func() {
		var hostLease *v1alpha1.HostLease

		BeforeEach(func() {
			hostLease = &v1alpha1.HostLease{
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "node-1",
					HostClass:  "openstack",
				},
			}
		})

		It("should set phase to Failed and PowerSynced to False on error", func() {
			reconciler.syncHostLeaseStatus(hostLease, nil, errors.New("ironic connection failed"))

			Expect(hostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseFailed))
			condition := hostLease.GetStatusCondition(v1alpha1.HostConditionPowerSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(v1alpha1.HostConditionReasonIronicAPIFailure))
			Expect(condition.Message).To(Equal("ironic connection failed"))
		})

		It("should set phase to Ready and PowerSynced to True when node is on", func() {
			node := &nodes.Node{PowerState: "power on"}
			reconciler.syncHostLeaseStatus(hostLease, node, nil)

			Expect(hostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseReady))
			Expect(hostLease.Status.PoweredOn).NotTo(BeNil())
			Expect(*hostLease.Status.PoweredOn).To(BeTrue())

			condition := hostLease.GetStatusCondition(v1alpha1.HostConditionPowerSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(v1alpha1.HostConditionReasonPowerOn))
		})

		It("should set phase to Ready and PowerSynced to True when node is off", func() {
			node := &nodes.Node{PowerState: "power off"}
			reconciler.syncHostLeaseStatus(hostLease, node, nil)

			Expect(hostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseReady))
			Expect(hostLease.Status.PoweredOn).NotTo(BeNil())
			Expect(*hostLease.Status.PoweredOn).To(BeFalse())

			condition := hostLease.GetStatusCondition(v1alpha1.HostConditionPowerSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(v1alpha1.HostConditionReasonPowerOff))
		})

		It("should set phase to Progressing when power state does not match desired", func() {
			hostLease.Spec.PoweredOn = boolPtr(true)
			node := &nodes.Node{PowerState: "power off"}
			reconciler.syncHostLeaseStatus(hostLease, node, nil)

			Expect(hostLease.Status.Phase).To(Equal(v1alpha1.HostLeasePhaseProgressing))
			Expect(hostLease.Status.PoweredOn).NotTo(BeNil())
			Expect(*hostLease.Status.PoweredOn).To(BeFalse())
		})

		It("should not modify status when node is nil and no error", func() {
			reconciler.syncHostLeaseStatus(hostLease, nil, nil)

			Expect(hostLease.Status.PoweredOn).To(BeNil())
			Expect(hostLease.Status.Conditions).To(BeEmpty())
		})
	})
})
