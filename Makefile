-include .isutools.mk

REMOTE_HOST ?=
REMOTE_DOCKER ?= docker
REMOTE_ROOT ?=
REMOTE_RESULTS ?= /tmp/isutools-results
REMOTE_RESULTS_SCP ?= $(REMOTE_RESULTS)

COMPOSE_FILES := -f $(REMOTE_ROOT)/webapp/compose.yml -f $(REMOTE_ROOT)/webapp/compose.isutools.yml
BENCH_IMAGE ?= isutools-private-isu-smoke-bench
APP_CONTAINER ?= private-isu-app-1
MYSQL_CONTAINER ?= private-isu-mysql-1
NETWORK ?= private-isu_my_network
LOCAL_PORT ?= 19191
RESULTS_DIR ?= $(HOME)/isutools-private-isu-results
PPROF_SECONDS ?= 30

SSH_OPTIONS := -o BatchMode=yes -o ConnectTimeout=15
SSH := ssh $(SSH_OPTIONS) $(REMOTE_HOST)
SCP := scp $(SSH_OPTIONS)

.DEFAULT_GOAL := help
.PHONY: help require-config status check bench pull-results verify-results open-results pprof tunnel dashboard build-tools

help:
	@printf '%s\n' \
	  'make check         private-isu, MySQL, and isutools readiness' \
	  'make status        show the existing Compose services' \
	  'make bench         run reset -> benchmark -> collect -> save, then copy results' \
	  'make pull-results  stage isutools artifacts remotely and fetch them with scp' \
	  'make verify-results summarize the newest local JSON artifact' \
	  'make open-results   open the newest self-contained HTML copied by SCP' \
	  'make pprof         capture a manual CPU profile and copy it locally' \
	  'make tunnel        forward localhost:$(LOCAL_PORT) (Ctrl-C to stop)' \
	  'make dashboard     open http://127.0.0.1:$(LOCAL_PORT)/ on this Mac' \
	  'make build-tools   build isutools agent, hub, pprof, and trajectory CLIs'

require-config:
	@test -n '$(REMOTE_HOST)' || { printf 'REMOTE_HOST is required; copy .isutools.mk.example to .isutools.mk\n' >&2; exit 2; }
	@test -n '$(REMOTE_ROOT)' || { printf 'REMOTE_ROOT is required; copy .isutools.mk.example to .isutools.mk\n' >&2; exit 2; }

status: require-config
	@$(SSH) '$(REMOTE_DOCKER) compose $(COMPOSE_FILES) ps'

check: require-config
	@command -v jq >/dev/null || { printf 'jq is required on the control PC\n' >&2; exit 2; }
	@command -v ssh >/dev/null || { printf 'ssh is required on the control PC\n' >&2; exit 2; }
	@command -v scp >/dev/null || { printf 'scp is required on the control PC\n' >&2; exit 2; }
	@$(SSH) 'curl -fsS --max-time 10 http://127.0.0.1/ >/dev/null'
	@$(SSH) 'curl -fsS --max-time 10 http://127.0.0.1:19191/json >/dev/null'
	@$(SSH) '$(REMOTE_DOCKER) exec $(MYSQL_CONTAINER) mysqladmin ping -h 127.0.0.1 -uroot -proot'
	@printf 'ready: private-isu, MySQL, and isutools\n'

bench: check
	@set -eu; \
	mkdir -p '$(RESULTS_DIR)'; \
	stamp=$$(date '+%Y%m%d-%H%M%S'); \
	log='$(RESULTS_DIR)'/bench-$$stamp.log; \
	headers=$$($(SSH) 'curl -fsS -D - -o /dev/null -X POST http://127.0.0.1:19191/reset'); \
	run_id=$$(printf '%s\n' "$$headers" | tr -d '\r' | awk 'tolower($$1) == "x-isutools-run-id:" { print $$2 }'); \
	printf 'isutools run: %s\n' "$${run_id:-unknown}"; \
	if ! $(SSH) '$(REMOTE_DOCKER) run --rm --network $(NETWORK) $(BENCH_IMAGE) /bin/benchmarker -t http://nginx -u /opt/userdata' 2>&1 | tee "$$log"; then \
	  $(SSH) 'curl -fsS -X POST http://127.0.0.1:19191/abort' >/dev/null 2>&1 || true; \
	  printf 'benchmark failed; isutools run aborted\n' >&2; \
	  exit 1; \
	fi; \
	result=$$(jq -Rrc 'fromjson? | select(type == "object" and has("score") and has("pass"))' "$$log" | tail -n 1); \
	if test -z "$$result"; then \
	  $(SSH) 'curl -fsS -X POST http://127.0.0.1:19191/abort' >/dev/null 2>&1 || true; \
	  printf 'benchmark JSON not found in %s; isutools run aborted\n' "$$log" >&2; \
	  exit 1; \
	fi; \
	score=$$(printf '%s\n' "$$result" | jq -er 'if (.score | type) == "number" then .score else error("score must be a number") end'); \
	pass=$$(printf '%s\n' "$$result" | jq -er 'if (.pass | type) == "boolean" then (.pass | tostring) else error("pass must be a boolean") end'); \
	if ! $(SSH) 'curl -fsS -X POST http://127.0.0.1:19191/collect' >/dev/null; then \
	  $(SSH) 'curl -fsS -X POST http://127.0.0.1:19191/abort' >/dev/null 2>&1 || true; \
	  exit 1; \
	fi; \
	$(SSH) "curl -fsS -X POST 'http://127.0.0.1:19191/save?score=$$score&pass=$$pass'"; \
	printf '\nbenchmark saved: score=%s pass=%s log=%s\n' "$$score" "$$pass" "$$log"; \
	make --no-print-directory pull-results

pull-results: require-config
	@$(SSH) 'mkdir -p "$(REMOTE_RESULTS_SCP)"'
	@$(SSH) '$(REMOTE_DOCKER) cp $(APP_CONTAINER):/tmp/. "$(REMOTE_RESULTS)"'
	@mkdir -p '$(RESULTS_DIR)'
	@$(SCP) -r '$(REMOTE_HOST):$(REMOTE_RESULTS_SCP)/.' '$(RESULTS_DIR)/'
	@printf 'results copied to %s\n' '$(RESULTS_DIR)'
	@find '$(RESULTS_DIR)' -maxdepth 1 -type f -print | sort

verify-results:
	@set -eu; \
	latest=$$(find '$(RESULTS_DIR)' -maxdepth 1 -type f -name '*.json' -print | sort | while IFS= read -r candidate; do \
		if jq -e '((.meta.schema_version | type) == "number") and ((.meta.time | type) == "string") and ((.meta.generation | type) == "number") and (((.meta.score | type) == "string") or ((.meta.score | type) == "number")) and ((.sql | type) == "array") and ((.http | type) == "array")' "$$candidate" >/dev/null 2>&1; then \
			printf '%s\n' "$$candidate"; \
		fi; \
	done | tail -n 1); \
	test -n "$$latest" || { printf 'no saved snapshot JSON artifacts in %s\n' '$(RESULTS_DIR)' >&2; exit 1; }; \
	printf 'artifact: %s\n' "$$latest"; \
	jq -e '{time:.meta.time,generation:.meta.generation,score:.meta.score,benchmark_pass:.meta.benchmark_pass,partial:.meta.partial,run_id:.meta.run.run_id,validity:.meta.run.validity,sql_rows:(.sql|length),http_rows:(.http|length),accesslog_lines:(.accesslog.lines // 0),accesslog_partial:(.accesslog.partial_lines // 0),profile_status:([.meta.health[]? | select(.collector=="profile")][0].status // "missing")}' "$$latest"

open-results:
	@set -eu; \
	latest=$$(find '$(RESULTS_DIR)' -maxdepth 1 -type f -name '*.html' -print | sort | tail -n 1); \
	test -n "$$latest" || { printf 'no HTML artifacts in %s\n' '$(RESULTS_DIR)' >&2; exit 1; }; \
	printf 'opening %s\n' "$$latest"; \
	open "$$latest"

pprof: check
	@set -eu; \
	case '$(PPROF_SECONDS)' in ''|*[!0-9]*) printf 'PPROF_SECONDS must be an integer\n' >&2; exit 2;; esac; \
	test '$(PPROF_SECONDS)' -ge 1 -a '$(PPROF_SECONDS)' -le 600 || { printf 'PPROF_SECONDS must be 1..600\n' >&2; exit 2; }; \
	mkdir -p '$(RESULTS_DIR)'; \
	stamp=$$(date '+%Y%m%d-%H%M%S'); \
	remote='$(REMOTE_RESULTS_SCP)'/cpu-$$stamp.pprof; \
	$(SSH) 'mkdir -p "$(REMOTE_RESULTS_SCP)"'; \
	$(SSH) "curl -fsS --max-time $$(( $(PPROF_SECONDS) + 15 )) -o '$$remote' 'http://127.0.0.1:19191/pprof/profile?seconds=$(PPROF_SECONDS)'"; \
	$(SCP) "$(REMOTE_HOST):$$remote" '$(RESULTS_DIR)/'; \
	printf 'profile copied to %s/cpu-%s.pprof\n' '$(RESULTS_DIR)' "$$stamp"

tunnel: require-config
	@if curl -fsS --max-time 2 http://127.0.0.1:$(LOCAL_PORT)/json >/dev/null 2>&1; then \
	  printf 'already available: http://127.0.0.1:$(LOCAL_PORT)/\n'; \
	else \
	  printf 'forwarding http://127.0.0.1:$(LOCAL_PORT)/ (Ctrl-C to stop)\n'; \
	  exec ssh $(SSH_OPTIONS) -o ExitOnForwardFailure=yes \
	    -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
	    -N -L $(LOCAL_PORT):127.0.0.1:19191 $(REMOTE_HOST); \
	fi

dashboard:
	@open 'http://127.0.0.1:$(LOCAL_PORT)/'

build-tools:
	@mkdir -p bin
	go build -o bin/isutools-agent ./cmd/isutools-agent
	go build -o bin/isutools-hub ./cmd/isutools-hub
	go build -o bin/isutools-pprof ./cmd/isutools-pprof
	go build -o bin/isutools-trajectory ./cmd/isutools-trajectory

.PHONY: test-adapters
test-adapters:
	cd adapters/echov4 && go test ./...
	cd adapters/echov5 && go test ./...
