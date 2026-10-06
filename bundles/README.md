# Instruction bundles

Markdown instruction bundles composed into each seat's instructions in this
order: organisation culture, team guidance (in declared team order, base
template first), then the seat's role (design §4.3).

| Bundle | Scope |
| --- | --- |
| `culture/culture.md` | Organisation culture; applies to every seat |
| `teams/base.md` | General team guidance; the base template every team extends |
| `teams/engineering.md` | Engineering team specialisation |
| `teams/accounting.md` | Accounting team specialisation |
| `roles/representative.md` | A human's personal representative |
| `roles/engineering_lead.md`, `roles/engineer.md`, `roles/reviewer.md` | Engineering roles |
| `roles/accountant.md` | Accounting role |

Bundles are published as ConfigMaps by `modules/instruction-bundle`. That
module outputs an immutable reference,
`configmap:<name>/<key>#sha256:<digest of the content>`, so changing a bundle
changes the digest. The change then shows up in the Terraform plan as a new
configuration revision for every affected seat.

Instructions guide behaviour. They do not grant authority: what a seat can do
is set by its grants, routes and sandbox, which the platform enforces. Tool
names below are the canonical dotted names from
`docs/tools.html`. Through MCP they appear with `.` replaced by
`_`, for example `memory_search`.
