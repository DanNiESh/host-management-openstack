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

// Package ironic provides a client for interacting with OpenStack Ironic bare metal API.
package ironic

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

// NodeClient is the interface for interacting with Ironic nodes.
type NodeClient interface {
	GetNode(ctx context.Context, nodeID string) (*nodes.Node, error)
	SetPowerState(ctx context.Context, nodeID string, target TargetPowerState) error
	IsNodePowerTransitioning(node *nodes.Node) bool
	ListNodePorts(ctx context.Context, nodeID string) ([]ports.Port, error)
	AttachVIF(ctx context.Context, nodeID, vifID, baremetalPortID string) error
	DetachVIF(ctx context.Context, nodeID, vifID string) error
}

// Client talks to Ironic over REST via gophercloud.
type (
	Client struct {
		serviceClient *gophercloud.ServiceClient
	}

	TargetPowerState struct {
		powerstate nodes.TargetPowerState
	}
)

func (t TargetPowerState) String() string {
	return string(t.powerstate)
}

var (
	PowerOn       = TargetPowerState{nodes.PowerOn}
	PowerOff      = TargetPowerState{nodes.PowerOff}
	Rebooting     = TargetPowerState{nodes.Rebooting}
	SoftPowerOff  = TargetPowerState{nodes.SoftPowerOff}
	SoftRebooting = TargetPowerState{nodes.SoftRebooting}
)

// NewClient creates an Ironic client with no-auth (typical for in-cluster Ironic with no Keystone).
func NewClient() (*Client, error) {
	client, err := clientconfig.NewServiceClient(context.TODO(), "baremetal", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create baremetal client: %w", err)
	}
	if client.Endpoint == "" {
		return nil, fmt.Errorf("baremetal client has no endpoint configured")
	}
	return &Client{
		serviceClient: client,
	}, nil
}

// GetNode fetches a node by UUID or name from Ironic.
func (c *Client) GetNode(ctx context.Context, nodeID string) (*nodes.Node, error) {
	node, err := nodes.Get(ctx, c.serviceClient, nodeID).Extract()
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", nodeID, err)
	}
	return node, nil
}

func (c *Client) GetEndpoint() string {
	return c.serviceClient.Endpoint
}

// IsNodePowerTransitioning returns true if the node is currently transitioning
// to a new power state (i.e., TargetPowerState is non-empty).
func (c *Client) IsNodePowerTransitioning(node *nodes.Node) bool {
	return node.TargetPowerState != ""
}

// SetPowerState requests power on or off for the node via Ironic.
func (c *Client) SetPowerState(ctx context.Context, nodeID string, target TargetPowerState) error {
	res := nodes.ChangePowerState(ctx, c.serviceClient, nodeID, nodes.PowerStateOpts{Target: target.powerstate})
	if err := res.ExtractErr(); err != nil {
		return fmt.Errorf("failed to set power state on node %s: %w", nodeID, err)
	}
	return nil
}

// ListNodePorts returns all baremetal ports for a node.
func (c *Client) ListNodePorts(ctx context.Context, nodeID string) ([]ports.Port, error) {
	var allPorts []ports.Port
	err := ports.ListDetail(c.serviceClient, ports.ListOpts{NodeUUID: nodeID}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		portList, err := ports.ExtractPorts(page)
		if err != nil {
			return false, err
		}
		allPorts = append(allPorts, portList...)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list ports for node %s: %w", nodeID, err)
	}
	return allPorts, nil
}

// AttachVIF attaches a Neutron port (VIF) to a specific baremetal port on a node.
func (c *Client) AttachVIF(ctx context.Context, nodeID, vifID, baremetalPortID string) error {
	err := nodes.AttachVirtualInterface(ctx, c.serviceClient, nodeID, nodes.VirtualInterfaceOpts{
		ID:       vifID,
		PortUUID: baremetalPortID,
	}).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to attach VIF %s to node %s port %s: %w", vifID, nodeID, baremetalPortID, err)
	}
	return nil
}

// DetachVIF detaches a Neutron port (VIF) from a node.
func (c *Client) DetachVIF(ctx context.Context, nodeID, vifID string) error {
	err := nodes.DetachVirtualInterface(ctx, c.serviceClient, nodeID, vifID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to detach VIF %s from node %s: %w", vifID, nodeID, err)
	}
	return nil
}
