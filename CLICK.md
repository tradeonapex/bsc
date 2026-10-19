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

## The weekly pipeline

The workflow runs itself every **Tuesday at 04:23 UTC** and builds whatever
bnb-chain's latest *stable* release is, if we have not built it already. Nothing
is deployed by it: a published release only becomes the binary on a node when
somebody bumps `bsc_release_tag` + `bsc_geth_sha256` in `ansible-bsc` and runs
the playbook. The pipeline's job is to make the artifact ready, not to decide
that it should run.

Why it exists: for two months this workflow was `workflow_dispatch` only, the
fork sat on `v1.7.8-click1`, upstream shipped `v1.8.1` on 2026-10-16, and the
role went on downloading bnb-chain's prebuilt `geth_linux` — the exact thing
this repository was created to stop. A build that needs someone to remember it
is not a pipeline.

How the decision is made, in the `resolve` job:

| | |
|---|---|
| which ref | `repos/bnb-chain/bsc/releases/latest`, which excludes drafts and pre-releases |
| what it refuses | anything that is not a plain `vX.Y.Z`. `v1.8.0-alpha` must never be built unattended, and `/releases/latest` guarantees the flag, not the name |
| idempotence | if `<ref>-click1` is already published, a scheduled run is a no-op; a **dispatch** still fails loudly, because an operator who named that release wants to be told |
| immutability | the tag is resolved to a commit sha before the build starts, and the build asserts the checkout matches it. A tag that moved in between stops the run |

A dispatch overrides all of it: every input you fill in is passed through
untouched, so the manual path below behaves exactly as it did before.

### The source comes from upstream, not from this fork

The build checks out `bnb-chain/bsc` at the resolved ref — **not** this
repository. That is what makes an unattended build possible at all: `click`
holds only the tags that existed when the fork was made, so building `v1.8.1`
from here would first mean pushing that tag in, and a tag push whose tree
carries upstream's 12 workflow files is precisely what the branch model above
exists to avoid.

Nothing is lost by it, because `click` carries no code of ours — it is
upstream's tree with upstream's workflows removed and ours added. The day we
actually patch geth, point `repository:` back at this repo and build a
click-based branch; the release notes record the commit either way.

## Building by hand

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

**Nothing needs to be pushed to this repository.** Dispatch the workflow with
the new `upstream_ref`, or wait for Tuesday and let the schedule find it. The
build reads the ref from bnb-chain.

What a bump still needs is the decision, which is deliberately left to a human:

1. read upstream's release notes — a geth bump on a pruned-full node is not
   always a drop-in, and a hard fork has a date;
2. set `bsc_release_tag` and `bsc_geth_sha256` in `ansible-bsc` (the release
   notes print both, ready to paste);
3. run the playbook. The binary changing fires `notify: restart bsc-geth`, so
   this is a restart, not a config nudge.

`v1.8.1` (2026-10-16) is the live example of why step 1 is not automatable: it
is flagged **"Mandatory Update Required: Yes"**, but its audience is *Chapel
testnet* operators and the Jenner hard fork it carries has **no mainnet
activation date in that release**. `bsc-01` is a mainnet node. The artifact
should be built and sitting in a release; running it is a separate call.

Only carrying a patch of our own still needs the merge:

```sh
git fetch https://github.com/bnb-chain/bsc.git --tags
git checkout click
git merge v1.8.1             # upstream's workflow files stay deleted; resolve
                             # "deleted by us" with `git rm` if it complains
git push origin click
```

…and then the build must be pointed at this repository instead of upstream.

## This is a public fork running on self-hosted runners

A fork of a public repository cannot be made private, and the build runs on our
own nodes inside lat-fra2. The two facts together are the standing hazard here:
a workflow that triggers on `pull_request` would let anyone on the internet run
code on a cluster node.

What keeps that shut:

- the build workflow triggers on **`workflow_dispatch` and `schedule` only**.
  A dispatch requires write access to this repository and a schedule can only
  be fired by GitHub itself on the default branch, so neither is reachable by
  an outside contributor;
- `fork-pr-contributor-approval` is set to `all_external_contributors`, so no
  workflow runs from any outside PR without a maintainer approving it:

  ```sh
  gh api repos/tradeonapex/bsc/actions/permissions/fork-pr-contributor-approval
  ```

Do not add `pull_request`, `pull_request_target` or `issue_comment` triggers to
a workflow in this repository. If PR validation is ever wanted here, it belongs
on GitHub-hosted runners, which are free for a public repository.

## Why this builds on a GitHub-hosted runner

Our self-hosted ARC scale sets (`lat-fra2-runners` and friends) are registered at
the organisation and carry no `runnerGroup`, so they sit in the **Default** runner
group — and a runner group does not serve **public** repositories unless an org
admin explicitly allows it. This fork is public, so a job targeting
`lat-fra2-runners` queues forever: the listener stays healthy with idle runners
and simply never receives the message (`assigned job: 0`, `lastMessageID` frozen).
There is no error anywhere; it just never starts.

`ubuntu-22.04` sidesteps that entirely. Public repositories get unlimited free
standard GitHub-hosted minutes, and 2.35 is older than the glibc on the target
host, which is the safe direction for a dynamically linked binary. `bsc-01` runs
bnb-chain's own prebuilt `geth_linux` today, and upstream builds that on
`ubuntu-latest`, so the host's glibc is at least that new.

The `runner` input keeps `lat-fra2-runners` selectable for the day a runner group
that allows public repositories exists.
