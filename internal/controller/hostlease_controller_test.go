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
	baremetalports "github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	neutronnetworks "github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	neutronports "github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
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
	listNodePortsFunc            func(ctx context.Context, nodeID string) ([]baremetalports.Port, error)
	attachVIFFunc                func(ctx context.Context, nodeID, vifID, baremetalPortID string) error
	detachVIFFunc                func(ctx context.Context, nodeID, vifID string) error
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

func (m *mockIronicClient) ListNodePorts(ctx context.Context, nodeID string) ([]baremetalports.Port, error) {
	if m.listNodePortsFunc != nil {
		return m.listNodePortsFunc(ctx, nodeID)
	}
	return nil, nil
}

func (m *mockIronicClient) AttachVIF(ctx context.Context, nodeID, vifID, baremetalPortID string) error {
	if m.attachVIFFunc != nil {
		return m.attachVIFFunc(ctx, nodeID, vifID, baremetalPortID)
	}
	return nil
}

func (m *mockIronicClient) DetachVIF(ctx context.Context, nodeID, vifID string) error {
	if m.detachVIFFunc != nil {
		return m.detachVIFFunc(ctx, nodeID, vifID)
	}
	return nil
}

// mockNeutronClient implements neutron.NetworkClient for testing.
type mockNeutronClient struct {
	listNetworksFunc    func(ctx context.Context) ([]neutronnetworks.Network, error)
	findPortFunc        func(ctx context.Context, portName string) (*neutronports.Port, error)
	createPortFunc      func(ctx context.Context, name, networkID, deviceOwner string) (*neutronports.Port, error)
	deletePortFunc      func(ctx context.Context, portID string) error
	isPortOnNetworkFunc func(ctx context.Context, portID, networkID string) (bool, error)
}

func (m *mockNeutronClient) ListNetworks(ctx context.Context) ([]neutronnetworks.Network, error) {
	if m.listNetworksFunc != nil {
		return m.listNetworksFunc(ctx)
	}
	return nil, nil
}

func (m *mockNeutronClient) FindPort(ctx context.Context, portName string) (*neutronports.Port, error) {
	if m.findPortFunc != nil {
		return m.findPortFunc(ctx, portName)
	}
	return nil, nil
}

func (m *mockNeutronClient) CreatePort(ctx context.Context, name, networkID, deviceOwner string) (*neutronports.Port, error) {
	if m.createPortFunc != nil {
		return m.createPortFunc(ctx, name, networkID, deviceOwner)
	}
	return &neutronports.Port{ID: "new-port-id"}, nil
}

func (m *mockNeutronClient) DeletePort(ctx context.Context, portID string) error {
	if m.deletePortFunc != nil {
		return m.deletePortFunc(ctx, portID)
	}
	return nil
}

func (m *mockNeutronClient) IsPortOnNetwork(ctx context.Context, portID, networkID string) (bool, error) {
	if m.isPortOnNetworkFunc != nil {
		return m.isPortOnNetworkFunc(ctx, portID, networkID)
	}
	return false, nil
}

func boolPtr(b bool) *bool {
	return &b
}

var _ = Describe("HostLeaseReconciler", func() {
	var (
		reconciler  *HostLeaseReconciler
		mockIronic  *mockIronicClient
		mockNeutron *mockNeutronClient
		testScheme  *runtime.Scheme
		log         = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
	)

	BeforeEach(func() {
		logf.SetLogger(log)
		testScheme = runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(testScheme)).To(Succeed())

		mockIronic = &mockIronicClient{}
		mockNeutron = &mockNeutronClient{}
		reconciler = &HostLeaseReconciler{
			Scheme:          testScheme,
			IronicClient:    mockIronic,
			NeutronClient:   mockNeutron,
			RecheckInterval: 10 * time.Second,
		}
	})

	Describe("NewHostLeaseReconciler", func() {
		It("should use the provided recheck interval when positive", func() {
			r := NewHostLeaseReconciler(nil, testScheme, mockIronic, mockNeutron, 30*time.Second)
			Expect(r.RecheckInterval).To(Equal(30 * time.Second))
		})

		It("should use the default recheck interval when zero", func() {
			r := NewHostLeaseReconciler(nil, testScheme, mockIronic, mockNeutron, 0)
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

		It("should run network reconciliation even when power reconciliation fails", func() {
			hostLease := &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-power-fail-net-ok",
					Namespace: "default",
					Finalizers: []string{
						hostLeaseFinalizer,
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID:   "node-power-fail",
					HostClass:    hostClass,
					NetworkClass: "openstack",
					PoweredOn:    boolPtr(true),
					NetworkInterfaces: []v1alpha1.NetworkInterfaceSpec{
						{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "test-net"},
					},
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
				return errors.New("ironic power failure")
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "net-id-1", Name: "test-net"},
				}, nil
			}
			mockNeutron.findPortFunc = func(_ context.Context, _ string) (*neutronports.Port, error) {
				return nil, nil
			}
			mockNeutron.createPortFunc = func(_ context.Context, _, networkID, _ string) (*neutronports.Port, error) {
				return &neutronports.Port{ID: "new-port", NetworkID: networkID}, nil
			}

			networkAttached := false
			mockIronic.attachVIFFunc = func(_ context.Context, _, _, _ string) error {
				networkAttached = true
				return nil
			}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      hostLease.Name,
					Namespace: hostLease.Namespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ironic power failure"))
			Expect(networkAttached).To(BeTrue())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(reconciler.Get(context.Background(), types.NamespacedName{
				Name:      hostLease.Name,
				Namespace: hostLease.Namespace,
			}, updatedHostLease)).To(Succeed())
			Expect(updatedHostLease.Status.NetworkInterfaces).To(HaveLen(1))
			Expect(updatedHostLease.Status.NetworkInterfaces[0].Network).To(Equal("test-net"))
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

	Describe("reconcileNetwork", func() {
		var (
			ctx       context.Context
			hostLease *v1alpha1.HostLease
		)

		BeforeEach(func() {
			ctx = context.Background()
			hostLease = &v1alpha1.HostLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostlease-network",
					Namespace: "default",
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID:   "node-net-1",
					HostClass:    hostClass,
					NetworkClass: "openstack",
				},
			}
		})

		It("should skip when networkClass is not openstack", func() {
			hostLease.Spec.NetworkClass = "other"
			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())

			listCalled := false
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				listCalled = true
				return nil, nil
			}
			Expect(listCalled).To(BeFalse())
		})

		It("should skip when networkClass is empty", func() {
			hostLease.Spec.NetworkClass = ""
			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip when networkInterfaces is empty and no VIFs attached", func() {
			hostLease.Spec.NetworkInterfaces = nil
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostLease.Status.NetworkInterfaces).To(BeEmpty())
		})

		It("should detach VIF and delete port when networkInterfaces is empty and VIF is attached", func() {
			hostLease.Spec.NetworkInterfaces = nil
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{
						UUID:    "bm-port-1",
						Address: "aa:bb:cc:dd:ee:f1",
						InternalInfo: map[string]any{
							"tenant_vif_port_id": "neutron-port-1",
						},
					},
				}, nil
			}

			var detachedVIF string
			mockIronic.detachVIFFunc = func(_ context.Context, nodeID, vifID string) error {
				detachedVIF = vifID
				return nil
			}

			var deletedPort string
			mockNeutron.deletePortFunc = func(_ context.Context, portID string) error {
				deletedPort = portID
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(detachedVIF).To(Equal("neutron-port-1"))
			Expect(deletedPort).To(Equal("neutron-port-1"))
			Expect(hostLease.Status.NetworkInterfaces).To(BeEmpty())
		})

		It("should attach VIF when networkInterfaces specifies a network", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "net-id-1", Name: "private-net"},
				}, nil
			}
			mockNeutron.findPortFunc = func(_ context.Context, _ string) (*neutronports.Port, error) {
				return nil, nil
			}
			mockNeutron.createPortFunc = func(_ context.Context, name, networkID, _ string) (*neutronports.Port, error) {
				Expect(networkID).To(Equal("net-id-1"))
				return &neutronports.Port{ID: "new-neutron-port", NetworkID: "net-id-1"}, nil
			}

			var attachedVIF, attachedBMPort string
			mockIronic.attachVIFFunc = func(_ context.Context, _, vifID, bmPortID string) error {
				attachedVIF = vifID
				attachedBMPort = bmPortID
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(attachedVIF).To(Equal("new-neutron-port"))
			Expect(attachedBMPort).To(Equal("bm-port-1"))
			Expect(hostLease.Status.NetworkInterfaces).To(HaveLen(1))
			Expect(hostLease.Status.NetworkInterfaces[0].MACAddress).To(Equal("aa:bb:cc:dd:ee:f1"))
			Expect(hostLease.Status.NetworkInterfaces[0].Network).To(Equal("private-net"))
		})

		It("should reuse existing neutron port if found", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "net-id-1", Name: "private-net"},
				}, nil
			}
			mockNeutron.findPortFunc = func(_ context.Context, _ string) (*neutronports.Port, error) {
				return &neutronports.Port{ID: "existing-port", NetworkID: "net-id-1"}, nil
			}

			createCalled := false
			mockNeutron.createPortFunc = func(_ context.Context, _, _, _ string) (*neutronports.Port, error) {
				createCalled = true
				return nil, nil
			}

			var attachedVIF string
			mockIronic.attachVIFFunc = func(_ context.Context, _, vifID, _ string) error {
				attachedVIF = vifID
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(createCalled).To(BeFalse())
			Expect(attachedVIF).To(Equal("existing-port"))
		})

		It("should skip interface when no matching baremetal port found", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:ff", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}

			attachCalled := false
			mockIronic.attachVIFFunc = func(_ context.Context, _, _, _ string) error {
				attachCalled = true
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(attachCalled).To(BeFalse())
			Expect(hostLease.Status.NetworkInterfaces).To(BeEmpty())
		})

		It("should no-op when VIF is already attached to correct network", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{
						UUID:    "bm-port-1",
						Address: "aa:bb:cc:dd:ee:f1",
						InternalInfo: map[string]any{
							"tenant_vif_port_id": "existing-neutron-port",
						},
					},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "net-id-1", Name: "private-net"},
				}, nil
			}
			mockNeutron.isPortOnNetworkFunc = func(_ context.Context, portID, networkID string) (bool, error) {
				Expect(portID).To(Equal("existing-neutron-port"))
				Expect(networkID).To(Equal("net-id-1"))
				return true, nil
			}

			attachCalled := false
			mockIronic.attachVIFFunc = func(_ context.Context, _, _, _ string) error {
				attachCalled = true
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(attachCalled).To(BeFalse())
			Expect(hostLease.Status.NetworkInterfaces).To(HaveLen(1))
			Expect(hostLease.Status.NetworkInterfaces[0].Network).To(Equal("private-net"))
		})

		It("should switch network when VIF is attached to wrong network", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "new-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{
						UUID:    "bm-port-1",
						Address: "aa:bb:cc:dd:ee:f1",
						InternalInfo: map[string]any{
							"tenant_vif_port_id": "old-neutron-port",
						},
					},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "old-net-id", Name: "old-net"},
					{ID: "new-net-id", Name: "new-net"},
				}, nil
			}
			mockNeutron.isPortOnNetworkFunc = func(_ context.Context, _, _ string) (bool, error) {
				return false, nil
			}
			mockNeutron.findPortFunc = func(_ context.Context, _ string) (*neutronports.Port, error) {
				return nil, nil
			}

			var detachedVIF, deletedPort string
			mockIronic.detachVIFFunc = func(_ context.Context, _, vifID string) error {
				detachedVIF = vifID
				return nil
			}
			mockNeutron.deletePortFunc = func(_ context.Context, portID string) error {
				deletedPort = portID
				return nil
			}
			mockNeutron.createPortFunc = func(_ context.Context, _, networkID, _ string) (*neutronports.Port, error) {
				return &neutronports.Port{ID: "new-neutron-port", NetworkID: networkID}, nil
			}

			var attachedVIF string
			mockIronic.attachVIFFunc = func(_ context.Context, _, vifID, _ string) error {
				attachedVIF = vifID
				return nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).NotTo(HaveOccurred())
			Expect(detachedVIF).To(Equal("old-neutron-port"))
			Expect(deletedPort).To(Equal("old-neutron-port"))
			Expect(attachedVIF).To(Equal("new-neutron-port"))
			Expect(hostLease.Status.NetworkInterfaces).To(HaveLen(1))
			Expect(hostLease.Status.NetworkInterfaces[0].Network).To(Equal("new-net"))
		})

		It("should return error when network not found", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "nonexistent-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return []neutronnetworks.Network{
					{ID: "net-id-1", Name: "other-net"},
				}, nil
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nonexistent-net"))
		})

		It("should return error when ListNodePorts fails", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return nil, errors.New("ironic list ports failed")
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ironic list ports failed"))
		})

		It("should return error when ListNetworks fails", func() {
			hostLease.Spec.NetworkInterfaces = []v1alpha1.NetworkInterfaceSpec{
				{MACAddress: "aa:bb:cc:dd:ee:f1", Network: "private-net"},
			}
			mockIronic.listNodePortsFunc = func(_ context.Context, _ string) ([]baremetalports.Port, error) {
				return []baremetalports.Port{
					{UUID: "bm-port-1", Address: "aa:bb:cc:dd:ee:f1"},
				}, nil
			}
			mockNeutron.listNetworksFunc = func(_ context.Context) ([]neutronnetworks.Network, error) {
				return nil, errors.New("neutron API error")
			}

			err := reconciler.reconcileNetwork(ctx, hostLease, log)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("neutron API error"))
		})
	})
})
