# Engineering Playbook

Tool-agnostic engineering guidance for `terraform-provider-oras`.

This is not a second multi-agent framework. The runtime (currently OpenCode + oh-my-opencode-slim) supplies orchestration. This directory supplies project-specific knowledge and procedure.

- `principles/`: stable invariants.
- `roles/`: only project-specific roles missing from the runtime.
- `skills/`: domain knowledge/checklists.
- `workflows/`: repeatable procedures.
- `decisions/`: ADRs.
- `experiments/`: evidence-producing experiments.

## Authority
When sources disagree prefer:
1. current source and executable tests;
2. accepted ADRs/current docs;
3. principles/skills;
4. persistent memory;
5. model prior knowledge.

External protocol/registry claims require current authoritative evidence.

## Knowledge promotion
Engram remembers transient discoveries. Git consolidates durable knowledge through tests, docs, ADRs and skills.
