# host-management-openstack

Kubernetes controller that manages bare metal hosts via OpenStack Ironic. Watches HostLease CRs (defined in [bare-metal-operator](https://github.com/osac-project/bare-metal-operator)) and reconciles power state with Ironic.

## Description

This controller filters HostLease CRs where `spec.hostClass == "openstack"` and uses `spec.externalID` as the Ironic node UUID. It reconciles the desired power state (`spec.poweredOn`) against the actual Ironic node power state, polling until they match.

Key behaviors:
- `spec.poweredOn = true` — powers on the node via Ironic
- `spec.poweredOn = false` — powers off the node via Ironic
- `spec.poweredOn = nil` — unmanaged; skips power reconciliation
- Sets `status.conditions[PowerSynced]` to reflect sync state
- Requeues with a configurable interval (`HOST_RECHECK_INTERVAL`, default 60s) until power state matches

## Running Tests

```bash
# Run all unit tests
go test ./internal/... ./cmd/...

# Run controller tests only
go test ./internal/controller/ -v

# Run ironic client tests only
go test ./internal/ironic/ -v
```

## Test Locally (with Ironic)

Run the controller on your machine and point it at your Ironic API. You need a Kubernetes API (e.g. a `kind` cluster) so the controller can watch HostLease CRs.

### 1. Create a kind cluster

```bash
kind create cluster --name host-mgmt-test
```

### 2. Install the HostLease CRD from bare-metal-operator

The HostLease CR is defined in the [bare-metal-operator](https://github.com/osac-project/bare-metal-operator).

```bash
kubectl apply -f <path-to-bare-metal-operator>/config/crd/bases/osac.openshift.io_hostleases.yaml
```

### 3. Run the controller

The controller uses standard OpenStack authentication via `OS_CLOUD` (or `OS_*` env vars). Configure a `clouds.yaml` with your Ironic endpoint, then:

```bash
OS_CLOUD=<your-cloud-name> make run
```

### 4. Apply a sample HostLease CR

In another terminal:

```bash
kubectl apply -f config/samples/v1alpha1_hostlease.yaml
```

### 5. Verify

```bash
# Check HostLease status
kubectl get hostlease hostlease-sample -n osac-namespace -o yaml

# Watch controller logs for reconciliation
```

## Getting Started

### Prerequisites
- go version v1.25.0+
- podman (or docker)
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster
- Access to an OpenStack Ironic endpoint

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make image-build image-push IMG=<some-registry>/host-management-openstack:tag
```

**Install the HostLease CRD (from bare-metal-operator):**

```sh
kubectl apply -f <path-to-bare-metal-operator>/config/crd/bases/osac.openshift.io_hostleases.yaml
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/host-management-openstack:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Contributing

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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
