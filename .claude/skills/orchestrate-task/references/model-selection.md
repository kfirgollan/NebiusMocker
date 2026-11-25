# Choosing a model per sub-task

Spend tokens where judgment is needed. Default to **Sonnet**. Go down to Haiku for mechanical work, and up to Opus
only for real ambiguity or high-stakes integration.

| Model | Use for | Examples in this repo |
|---|---|---|
| **haiku** | Mechanical, fully specified, easy to check; lookups and summaries | Run a test suite and summarize failures; grep where something is used; rename across files; regenerate docs tables; bump version pins; read upstream docs and extract a field list; write boilerplate from an exact template |
| **sonnet** | Well-specified implementation that follows an existing pattern | A new resource kind modeled on IAM groups; DAL repos; api views; table-driven tests; a Terraform e2e scenario following `test/terraform/iam`; docs pages; focused bug fixes with a known cause |
| **opus** | Design, ambiguity, cross-cutting or subtle work | Architecture of a new backend integration (auth fronts, proxies); debugging with an unknown cause or flaky concurrency; changes to `pkg/` shared by many areas; reconciling conflicting upstream behavior; final integration review of a large diff |

## Rules of thumb

- **Explain cost vs. do cost:** if writing the brief takes longer than doing the work, do it yourself.
- **Escalate on evidence:** one failed attempt with the exact error, then move up one tier, passing the failure along.
  Don't start at Opus "to be safe".
- **Don't retry the same tier blindly.** A second identical brief gets the same result. Change the brief or the tier.
- **Parallelize only file-disjoint work.** Use at most 3 concurrent workers, since each one costs context to brief and review.
- **Keep reports short:** ask for ≤ 15-line reports, so you're not reading transcripts.
- **Reuse a worker that already has context** (SendMessage to it) for follow-ups on the same files, instead of a new one.
