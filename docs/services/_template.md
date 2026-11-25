# <Area name>

> Module: `services/<area>` · Image: `nebius-mocker-<area>` · `NEBIUS_MOCKER_SERVICES=<area>`
> M1 tier: **Tx** · Later: …

## Overview

What this service is in Nebius (one paragraph). Link the public Nebius docs page.

| Resource kind | Message | NID prefix | Parent | Lifecycle states |
|---|---|---|---|---|

## Emulation design

### Resource model and validation
Domain rules beyond `buf.validate`, cross-resource references and their scope rules.

### Lifecycle
Create/Update/Delete paths and custom verbs, with timing keys.

### Side effects
What this module does to other modules' resources, or reacts to.

### Seed catalogs and synthesized outputs
Default data, and how output-only fields are computed.

### Permissions and quotas
Permission strings and quota bindings.

## Terraform

| TF object | RPCs |
|---|---|

## Configuration

| Env | Default | Description |
|---|---|---|

## Open questions
Behavior to confirm against real Nebius in parity recording.

## API surface
(generated)
