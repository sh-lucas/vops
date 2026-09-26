# writing and interacting with the user

Don't spam comments, don't add linebreaks in the middle of sentences, and keep your language as concise as possible (we all have TDAH today).

README.md has the expected features, AGENTS.md has system instructions, reference/ has the styleguides and general documentation for anybody to understand how vops works.
Read it, and keep it up to date.

Map: reference/decisions.md records every design decision (update it when you make or change one), reference/compose.md the supported compose subset, reference/architecture.md the package map. `just test` runs everything (needs podman). TODO.md has the next ideas with design sketches.
SQL lives in internal/store/queries.sql and internal/store/migrations/ (new file per change, never edit an applied one); run `just gen` (sqlc) and commit the generated internal/store/queries/.

On simple stuff, use subagents, but keep them around for the next batch of features and give instruction by instruction to avoid context creep. sonnet 5 is recommended in this case. For complex stuff, orchestrate: a single agent that handles the whole thing, and you just think about the high level product and UX.
