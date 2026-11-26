# NebiusMocker documentation

NebiusMocker is an open-source, local emulator of the [Nebius AI Cloud](https://nebius.com) public API, built in Go.
LocalStack does this for AWS. NebiusMocker does it for Nebius: it serves the same gRPC services as the real cloud and
behaves the way they do. That covers authentication, permissions, quotas, async operations and resource lifecycles.
The Nebius CLI, SDKs and Terraform provider can therefore run against a laptop or a CI container
instead of a real tenant.

> Status: early, with working services. See the [Terraform support matrix](terraform.md) and each service doc's **Implementation status** section.

## Map

| Section | What it covers |
|---|---|
| [Architecture](architecture/README.md) | Goals, system design, Go module layout, request pipeline, IAM, state, lifecycles, quotas, deployment, test harness, milestones, decisions |
| [Terraform support](terraform.md) | Using the official Terraform provider against the mock, and which of its 116 objects are supported today versus the official provider docs |
| [Services catalog](services/README.md) | Every public Nebius service (91 gRPC services, 488 RPCs), how each one will be emulated, and what the Terraform provider needs from it |

## Sources of truth

Every API fact in these docs comes from these upstream repositories, pinned at the time of writing (2026-11-25):

| Upstream | Use | Pinned revision |
|---|---|---|
| [`nebius/api`](https://github.com/nebius/api) (MIT) | `.proto` definitions, endpoint map, API conventions | `45221893` |
| [`nebius/gosdk`](https://github.com/nebius/gosdk) (MIT) | Generated Go types **and gRPC server stubs**, reset-mask library, NID parser, resolver logic | `248592ef` |
| [`nebius/terraform-provider-nebius`](https://github.com/nebius/terraform-provider-nebius) (Apache-2.0) | The RPCs each Terraform resource calls, provider configuration and routing | `09355968` (v0.6.91) |

When upstream changes, regenerate the API tables in `docs/services/` (see
[architecture/12-test-harness.md § API drift](architecture/12-test-harness.md#api-drift-detection)).
