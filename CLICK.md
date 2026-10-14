# Click fork of bnb-chain/bsc

This fork exists so the `geth` binary on `bsc-01` is one **we** built, instead of
bnb-chain's prebuilt `geth_linux` asset fetched from their release at deploy
time.

Consumer: [`ansible-bsc`](https://github.com/tradeonapex/ansible-bsc)
(`bsc_release_tag`, `bsc_geth_url`, `bsc_mainnet_config_url`, `bsc_geth_sha256`).

## Branch model

| Branch | What it is |
|---|---|
| `master` | The upstream tree, **frozen at fork time**. Never pushed to. |
| `click` | **Default branch.** Upstream tree + `.github/workflows/click-bsc-release.yml`, with upstream's own `.github/workflows/` removed. |

Two reasons `click` is the default and not a side branch:

1. `workflow_dispatch` only registers a workflow that lives on the default
   branch.
2. Upstream has 12 workflows, **11 of them on `push`**, and `release.yml` plus
   `pre-release.yml` trigger on `push: tags`. With Actions enabled on a fork,
   pushing a release tag whose tree carries those files would fire bnb-chain's
   own release pipeline from our repo. So `master` is frozen, our branch carries
   no upstream workflow, our release tags target `click`, and every pre-existing
   workflow is additionally disabled through the API as a second layer:

   ```sh
   gh api repos/tradeonapex/bsc/actions/workflows -q '.workflows[] | "\(.id) \(.name)"'
   gh api -X PUT repos/tradeonapex/bsc/actions/workflows/<id>/disable
   ```

**Do not run `gh repo sync`.** It pushes to `master`, which is exactly the thing
the model above avoids.

## Building

Actions → *click · geth_linux → release* → Run workflow.

| Input | Note |
|---|---|
| `upstream_ref` | Tag, branch or SHA. The role tracks `v1.7.8`; `v1.8.0-alpha` exists upstream but is a pre-release. |
| `suffix` | Release-tag suffix: `<upstream_ref>-<suffix>`, e.g. `v1.7.8-click1`. Bump it (`click2`, …) rather than replacing an asset that a host may already be running. |
| `publish` | Off builds and keeps the artifact for 7 days without creating a release. |

The release carries three assets:

- `geth_linux` — the binary from `make geth`, named as upstream names theirs;
- `geth_linux.sha256` — filled into `bsc_geth_sha256`, which the role already
  passes to `get_url`'s `checksum:`. That hook was in the defaults unused from
  the start; building ourselves is what makes it meaningful;
- `mainnet.zip` — upstream's `config.toml` + `genesis.json`, re-attached
  unchanged, so `bsc_release_tag` alone decides both downloads in the role
  instead of pointing half of them back at bnb-chain.

### glibc direction

The runner image is based on `ghcr.io/actions/actions-runner` (Ubuntu 22.04,
glibc 2.35) and the hosts are newer, so the binary is forward-compatible. The
build prints `ldd` and the highest `GLIBC_` symbol version the binary needs, and
records it in the release notes — the day that stops being true, it shows up
here rather than as a start-up failure on the node.

## Bumping to a new upstream version

```sh
git fetch https://github.com/bnb-chain/bsc.git --tags
git checkout click
git merge v1.8.0             # upstream's workflow files stay deleted; resolve
                             # "deleted by us" with `git rm` if it complains
git push origin click
```

Then dispatch the workflow, and update `bsc_release_tag` + `bsc_geth_sha256` in
`ansible-bsc`. Note that a geth minor bump on a pruned-full node is not always a
drop-in: check upstream's release notes for a required resync or a hard fork
date before rolling it out.

## This is a public fork running on self-hosted runners

A fork of a public repository cannot be made private, and the build runs on our
own nodes inside lat-fra2. The two facts together are the standing hazard here:
a workflow that triggers on `pull_request` would let anyone on the internet run
code on a cluster node.

What keeps that shut:

- the build workflow is **`workflow_dispatch` only**, which requires write
  access to this repository;
- `fork-pr-contributor-approval` is set to `all_external_contributors`, so no
  workflow runs from any outside PR without a maintainer approving it:

  ```sh
  gh api repos/tradeonapex/bsc/actions/permissions/fork-pr-contributor-approval
  ```

Do not add `pull_request`, `pull_request_target` or `issue_comment` triggers to
a workflow in this repository. If PR validation is ever wanted here, it belongs
on GitHub-hosted runners, which are free for a public repository.
