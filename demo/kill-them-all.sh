#!/usr/bin/env bash
# Deploy four demo workloads that die in four different ways, then wait until
# each death shows up in the pod status. The fourth (second-life.yaml) starts
# healthy and is broken by a bad release (MODE=turbo) while its old Pods live on.
# Use --cleanup to delete the demo namespace.
#
# Uses the current kubectl context. Run `kubectl config current-context` first;
# for the kind cluster from demo/kind-config.yaml the context is kind-seance.
set -euo pipefail

NAMESPACE="seance-demo"
TIMEOUT_SECONDS=120
ROLLOUT_TIMEOUT="120s"
SECOND_LIFE_LABEL="app.kubernetes.io/name=second-life"
POLL_INTERVAL_SECONDS=5

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  echo "Usage: $0 [--cleanup]"
  echo "  (no args)   create namespace ${NAMESPACE}, apply the 4 demo workloads, release a bad"
  echo "              version of second-life, wait for deaths"
  echo "  --cleanup   delete namespace ${NAMESPACE} (nothing else)"
}

require_kubectl() {
  if ! command -v kubectl >/dev/null 2>&1; then
    echo "error: kubectl not found in PATH" >&2
    exit 1
  fi
}

cleanup() {
  echo "Deleting namespace ${NAMESPACE} (only this namespace is touched)..."
  kubectl delete namespace "${NAMESPACE}" --ignore-not-found
  echo "Done."
}

# Prints one line per container: waiting reason, last terminated reason, current terminated reason.
# second-life is left out: its bad release is watched by second_life_states.
pod_states() {
  kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/name!=second-life" -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}waiting={.state.waiting.reason} lastTerminated={.lastState.terminated.reason} terminated={.state.terminated.reason}{"\n"}{end}{end}'
}

# Succeeds when the given pattern appears in the pod states output.
seen() {
  grep -qE "$1" <<< "$2"
}

# Prints one line per second-life Pod: name, waiting reason, last terminated reason.
second_life_states() {
  kubectl get pods -n "${NAMESPACE}" -l "${SECOND_LIFE_LABEL}" -o jsonpath='{range .items[*]}{.metadata.name}{range .status.containerStatuses[*]} waiting={.state.waiting.reason} lastTerminated={.lastState.terminated.reason}{end}{"\n"}{end}'
}

# Deploys the healthy version 1 of second-life, waits until it is rolled out,
# then releases the bad version 2 (MODE=turbo). maxUnavailable: 0 keeps the
# version 1 Pods alive while the version 2 Pod crashes.
second_life() {
  kubectl apply -f "${SCRIPT_DIR}/second-life.yaml"
  echo "Waiting up to ${ROLLOUT_TIMEOUT} for the healthy second-life v1..."
  kubectl rollout status deployment/second-life -n "${NAMESPACE}" --timeout="${ROLLOUT_TIMEOUT}"
  echo "Releasing a bad second-life v2 (MODE=turbo)..."
  kubectl set env deployment/second-life -n "${NAMESPACE}" MODE=turbo
}

# Prints one line per BackOff event: type reason involvedObject.name.
# Crash-looping containers sit in terminated=Error most of the time, so CrashLoopBackOff
# in pod states is only seen briefly. The BackOff events stay around longer.
backoff_events() {
  kubectl get events -n "${NAMESPACE}" --field-selector reason=BackOff \
    -o jsonpath='{range .items[*]}{.type} {.reason} {.involvedObject.name}{"\n"}{end}'
}

deploy_and_wait() {
  require_kubectl

  echo "Ensuring namespace ${NAMESPACE} exists..."
  kubectl create ns "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

  echo "Applying demo workloads..."
  kubectl apply -f "${SCRIPT_DIR}/oom.yaml"
  kubectl apply -f "${SCRIPT_DIR}/crashloop.yaml"
  kubectl apply -f "${SCRIPT_DIR}/imagepull.yaml"
  second_life

  echo "Waiting up to ${TIMEOUT_SECONDS}s for OOMKilled, CrashLoopBackOff, ImagePull errors and the bad release..."
  local start now elapsed out second
  local oom=no crash=no pull=no bad=no events
  start="$(date +%s)"

  while true; do
    if ! out="$(pod_states)"; then
      echo "  warning: kubectl get pods failed, retrying" >&2
      out=""
    fi
    if ! second="$(second_life_states)"; then
      echo "  warning: kubectl get pods (second-life) failed, retrying" >&2
      second=""
    fi
    if ! events="$(backoff_events)"; then
      echo "  warning: kubectl get events failed, retrying" >&2
      events=""
    fi

    # OOMKilled shows up in lastState (after a restart) or in the current terminated state.
    if [[ "${oom}" == "no" ]] && (seen 'lastTerminated=OOMKilled|terminated=OOMKilled' "${out}"); then
      oom=yes
      echo "  [+] OOMKilled seen"
    fi
    if [[ "${crash}" == "no" ]] && (seen 'waiting=CrashLoopBackOff' "${out}"); then
      crash=yes
      echo "  [+] CrashLoopBackOff seen (container waiting state)"
    elif [[ "${crash}" == "no" ]] && (seen '^Warning BackOff restless-spirit' "${events}"); then
      crash=yes
      echo "  [+] CrashLoopBackOff seen (Warning BackOff event for restless-spirit)"
    fi
    if [[ "${pull}" == "no" ]] && (seen 'waiting=(ErrImagePull|ImagePullBackOff)' "${out}"); then
      pull=yes
      echo "  [+] ErrImagePull/ImagePullBackOff seen"
    fi

    # The bad release: a second-life Pod crash-looping while another one still lives.
    if [[ "${bad}" == "no" ]] && (seen 'waiting=CrashLoopBackOff|lastTerminated=Error' "${second}" || seen '^Warning BackOff second-life' "${events}"); then
      bad=yes
      echo "  [+] Bad release of second-life seen (the v1 Pods stay alive)"
    fi

    if [[ "${oom}" == "yes" && "${crash}" == "yes" && "${pull}" == "yes" && "${bad}" == "yes" ]]; then
      echo "All four deaths observed. Summon them with the app."
      return 0
    fi

    now="$(date +%s)"
    elapsed=$((now - start))
    if (( elapsed >= TIMEOUT_SECONDS )); then
      echo "Timed out after ${TIMEOUT_SECONDS}s. Missing:" >&2
      [[ "${oom}" == "no" ]] && echo "  - OOMKilled" >&2
      [[ "${crash}" == "no" ]] && echo "  - CrashLoopBackOff" >&2
      [[ "${pull}" == "no" ]] && echo "  - ErrImagePull/ImagePullBackOff" >&2
      [[ "${bad}" == "no" ]] && echo "  - second-life bad release (CrashLoopBackOff)" >&2
      echo "Current pod states:" >&2
      echo "${out:-  (no pods yet)}" >&2
      exit 1
    fi

    echo "  ... ${elapsed}s elapsed, still waiting"
    sleep "${POLL_INTERVAL_SECONDS}"
  done
}

main() {
  case "${1:-}" in
    "")
      deploy_and_wait
      ;;
    --cleanup)
      require_kubectl
      cleanup
      ;;
    -h|--help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
