# K8s Séance

Summon a dead K8s Pod's ghost and let it tell you how it died, then get an auto-generated postmortem.

Status: WIP

## Quick start (milestone 1)

Needs Docker, `kind`, `kubectl` and Go 1.23+. This runs the CLI against a local kind cluster. The Web UI is not built yet.

```sh
kind create cluster --name seance --config demo/kind-config.yaml
./demo/kill-them-all.sh          # creates namespace seance-demo with 3 pods that die in different ways
go run ./cmd/seance -n seance-demo
```

The CLI only reads from the cluster. Flags: `-n` namespace (default `default`), `-pod` one pod, `-tail` log lines kept (default 200), `-kubeconfig` path (default `$KUBECONFIG` or `~/.kube/config`), `-fixtures` offline mode (see below).

Offline, without a cluster (uses the recorded fixtures in `fixtures/`):

```sh
go run ./cmd/seance -fixtures
```

Record fixtures from a live demo cluster. This writes local JSON files only, it makes no cluster writes. The Pods are saved as `fixtures/oom.json`, `fixtures/crashloop.json` and `fixtures/imagepull.json`, and pods with other causes are skipped:

```sh
SEANCE_RECORD=1 go run ./cmd/seance -n seance-demo
```

`SEANCE_FIXTURES_DIR` changes the output directory (default `fixtures`).

Clean up with `./demo/kill-them-all.sh --cleanup`.

## Quick start

TODO

## Sample mode

TODO

## Read-only RBAC

TODO

## How we used Kiro

TODO
