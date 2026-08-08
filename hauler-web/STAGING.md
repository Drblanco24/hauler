# This directory is staged here temporarily

`hauler-web` is a **separate project** that belongs in its own repository
(`Drblanco24/hauler-web`). It is checked in here only because the session that
wrote it could not be granted access to that repository — adding it required an
approval that was not available — and an ephemeral container is not a safe
place to leave work.

It is a self-contained Go module (`github.com/Drblanco24/hauler-web`) with its
own `go.mod`. It does **not** import any hauler package; it drives the `hauler`
binary as a subprocess. Nothing outside this directory was modified, and
because Go excludes nested modules from `./...`, hauler's own build, vet, and
test targets are unaffected — verified with `go build ./...` and
`go list ./...` at the repository root.

## Moving it to its own repository

From the root of this repository, on this branch:

```bash
git subtree split --prefix=hauler-web -b hauler-web-only

git remote add hauler-web git@github.com:Drblanco24/hauler-web.git
git push hauler-web hauler-web-only:main
```

Then delete this directory and the branch it was staged on:

```bash
git rm -r hauler-web && git commit -m "chore: move hauler-web to its own repository"
```

The history of this directory is preserved by `subtree split`, so the two
commits that built it carry over intact.

## What is in it

See `README.md` for the current state. In short: the reconciliation core
(config, manifest parsing, the hauler CLI wrapper, the planner, and digest
resolution) is implemented and tested, the Postgres schema is written, and
`hauler-web plan` works today without a database.
