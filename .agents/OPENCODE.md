# OpenCode Integration

The playbook is tool-agnostic. Current runtime mapping:

- `oh-my-opencode-slim`: orchestration and built-in specialists.
- Ponytail: minimalism / unnecessary-work reduction.
- Engram: persistent working memory.
- RTK/tool optimization: runtime concern outside this repository.
- Do not duplicate explorer/designer/fixer/oracle roles here.
- `.agents/roles/adversary.md` supplies the project-specific missing role.

Suggested flow:
orchestrator → explorer/librarian + investigate → designer + design → fixer + implement → oracle/reviewer + review → explicit adversary review for concurrency-sensitive work → verify → human acceptance.

Engram is memory, not authority. Promote durable conclusions into Git.
