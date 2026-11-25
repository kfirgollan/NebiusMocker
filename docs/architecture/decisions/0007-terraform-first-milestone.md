# ADR-0007: Unmodified Terraform provider as the first acceptance target

**Context.** Terraform is the most common way to drive Nebius in automation. The provider source is public
(`nebius/terraform-provider-nebius`, Apache-2.0). Its configuration already supports endpoint overrides (`resolvers`,
`address_options`, `domain`, `token`), and it tolerates `UNIMPLEMENTED` for preflight checks.

**Decision.** Milestone 1 is "every resource, data source and ephemeral resource of the pinned registry provider passes
apply → empty plan → update → destroy against the mocker". The provider is not patched. The only client-side change is
a provider override file.

**Consequences.** The "empty second plan" invariant forces precise round-tripping of server defaults and output fields,
which is exactly the fidelity we want. The RPC set the provider uses (`tools/tfcoverage`) defines the M1 surface, and
other RPCs can stay at T0 until M4.
