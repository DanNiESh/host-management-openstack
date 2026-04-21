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

// Package neutron provides a client for interacting with OpenStack Neutron networking API.
package neutron

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

// NetworkClient is the interface for interacting with Neutron resources.
type NetworkClient interface {
	ListNetworks(ctx context.Context) ([]networks.Network, error)
	FindPort(ctx context.Context, portName string) (*ports.Port, error)
	CreatePort(ctx context.Context, name, networkID, deviceOwner string) (*ports.Port, error)
	DeletePort(ctx context.Context, portID string) error
	IsPortOnNetwork(ctx context.Context, portID, networkID string) (bool, error)
}

// Client talks to Neutron over REST via gophercloud.
type Client struct {
	serviceClient *gophercloud.ServiceClient
}

// NewClient creates a Neutron client using the configured OpenStack credentials.
func NewClient() (*Client, error) {
	client, err := clientconfig.NewServiceClient(context.TODO(), "network", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %w", err)
	}
	if client.Endpoint == "" {
		return nil, fmt.Errorf("network client has no endpoint configured")
	}
	return &Client{
		serviceClient: client,
	}, nil
}

// GetEndpoint returns the Neutron service endpoint URL.
func (c *Client) GetEndpoint() string {
	return c.serviceClient.Endpoint
}

// ListNetworks returns all networks from Neutron.
func (c *Client) ListNetworks(ctx context.Context) ([]networks.Network, error) {
	var allNetworks []networks.Network
	err := networks.List(c.serviceClient, nil).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		nets, err := networks.ExtractNetworks(page)
		if err != nil {
			return false, err
		}
		allNetworks = append(allNetworks, nets...)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}
	return allNetworks, nil
}

// FindPort finds a Neutron port by name. Returns nil if not found.
func (c *Client) FindPort(ctx context.Context, portName string) (*ports.Port, error) {
	var found *ports.Port
	err := ports.List(c.serviceClient, ports.ListOpts{Name: portName}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		portList, err := ports.ExtractPorts(page)
		if err != nil {
			return false, err
		}
		if len(portList) > 0 {
			found = &portList[0]
			return false, nil // stop paging
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find port %s: %w", portName, err)
	}
	return found, nil
}

// CreatePort creates a new Neutron port on the given network.
func (c *Client) CreatePort(ctx context.Context, name, networkID, deviceOwner string) (*ports.Port, error) {
	adminStateUp := true
	port, err := ports.Create(ctx, c.serviceClient, ports.CreateOpts{
		Name:         name,
		NetworkID:    networkID,
		AdminStateUp: &adminStateUp,
		DeviceOwner:  deviceOwner,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create port %s: %w", name, err)
	}
	return port, nil
}

// DeletePort deletes a Neutron port by ID.
func (c *Client) DeletePort(ctx context.Context, portID string) error {
	err := ports.Delete(ctx, c.serviceClient, portID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to delete port %s: %w", portID, err)
	}
	return nil
}

// IsPortOnNetwork checks whether a Neutron port belongs to the given network.
func (c *Client) IsPortOnNetwork(ctx context.Context, portID, networkID string) (bool, error) {
	port, err := ports.Get(ctx, c.serviceClient, portID).Extract()
	if err != nil {
		return false, fmt.Errorf("failed to get port %s: %w", portID, err)
	}
	return port.NetworkID == networkID, nil
}
