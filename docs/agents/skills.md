# Agent skill workflows

Use `mattpocock-skills` to guide matching work in Hypercall. The plugin is installed
in the developer environment; its instructions remain in the plugin, rather than
being copied into this repository. [AGENTS.md](../../AGENTS.md) owns repo-wide rules.

## Selecting and loading a skill

1. Match the user's task to the workflow below. An explicitly requested skill
   takes priority over an inferred workflow.
2. Locate its `SKILL.md` through the active skill catalog. For installed skills
   omitted from that catalog, inspect the installed plugin manifest and read the
   declared skill file and required references from its actual installation.
3. Announce the selected workflow and follow its instructions within the authorized
   task. Skill availability does not authorize unrelated work or external writes.
4. If a required skill is unavailable, report that limitation. Use ordinary repo
   practices for optional skills rather than claiming to have followed them.

## Workflow routing

| Task | Skill | Hypercall application |
| --- | --- | --- |
| Challenge an architecture or plan | `grill-with-docs` | Load its `grilling` and `domain-modeling` workflows; resolve decisions in rounds and record settled terminology/trade-offs. |
| Clarify design decisions through an interview | `grilling` | Ask decisions whose prerequisites are settled, provide recommendations, and wait for answers before dependent questions. |
| Define terminology or document an architectural trade-off | `domain-modeling` | Maintain resolved domain language and justified ADRs; distinguish accepted decisions from proposals. |
| Turn an agreed conversation into a specification | `to-spec` | Ground the spec in the architecture and current development stage. |
| Break an agreed scope into implementation tickets | `to-tickets` | Use stage acceptance gates; include dependencies and independently verifiable outcomes. |
| Implement behavior or fix a bug | `tdd` | Use behavior-focused tests; real Linux/KVM tests qualify sandbox and snapshot claims. |
| Diagnose a failure or regression | `diagnosing-bugs` | Establish evidence and a reproducible failure before choosing the fix. |
| Design module boundaries | `codebase-design` | Keep upstream adapters behind Hypercall contracts and preserve one lifecycle owner. |
| Review code changes | `code-review` | Review both repo standards and the originating spec, against an explicit comparison base. |
| Research an unresolved technical question | `research` | Use primary sources and retain exact upstream revisions and reproducible evidence. |
| Edit agent instructions | `writing-for-agents` | Keep root guidance under 60 lines; use task-triggered pointers and one authoritative location per rule. |
| Write a pull request description | `pr` | Describe the resulting behavior, validation, and material limitations. |

Load any required companion skills and use delegation when the selected workflow
explicitly calls for it. Keep delegated work inside the same authorized scope.

## Repository integration

- Design source: [ARCHITECTURE.md](../ARCHITECTURE.md).
- Delivery stages and completion criteria: [DEVELOPMENT_PHASES.md](../DEVELOPMENT_PHASES.md).
- Domain language uses root `GLOSSARY.md`; decision records use `docs/adr/`.
  Create them lazily when terms or decisions are actually settled.
- An issue tracker has not been selected. When a tracker-backed workflow is
  requested, read `setup-matt-pocock-skills` and establish its repo configuration
  before publishing specifications, issues, labels, or comments.
- Keep the architecture review gate in force until the user reviews the design.
  Interviews, documentation, and planning can proceed within that boundary.
- Skills requesting throwaway prototypes do not override the production-quality
  goal. Use them only when the user explicitly requests a separate prototype.
- Preserve the grounding repositories, source pins, licenses, and attribution
  rules when applying any implementation or research workflow.

Plugin workflow adoption does not mark the architecture approved, complete any
development phase, configure an external tracker, or publish project information.
