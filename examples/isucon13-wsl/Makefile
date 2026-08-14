-include isutools.mk

REMOTE_HOST ?=
WSL_DISTRO ?= isucon13
LOCAL_PORT ?= 19196
REMOTE_ADMIN_PORT ?= 19196
GUEST_RELAY_PORT ?= 29196
WINDOWS_USER ?=
RESULTS_DIR ?= $(HOME)/isutools-isucon13-results
PPROF_SECONDS ?= 30

SSH_OPTIONS := -o BatchMode=yes -o ConnectTimeout=15
SSH := ssh $(SSH_OPTIONS) $(REMOTE_HOST)
SCP := scp $(SSH_OPTIONS)
WSL := /init /mnt/c/Windows/System32/cmd.exe /c wsl.exe -d $(WSL_DISTRO) --
STAGE_LINUX := /mnt/c/Users/$(WINDOWS_USER)/isutools-isucon13-results-stage

.DEFAULT_GOAL := help
.PHONY: help require-config status check bench pull-results verify-results pprof tunnel dashboard

help:
	@printf '%s\n' \
	  'make status          show WSL and required systemd services' \
	  'make check           check HTTPS, MySQL, and isutools readiness' \
	  'make bench           reset -> official bench -> collect -> save -> SCP' \
	  'make pull-results    copy staged artifacts to this PC with scp' \
	  'make verify-results  summarize the newest saved snapshot' \
	  'make pprof           capture a manual CPU profile (run mode must be off)' \
	  'make tunnel          forward dashboard to localhost:$(LOCAL_PORT)' \
	  'make dashboard       open the local dashboard URL'

require-config:
	@test -n '$(REMOTE_HOST)' || { printf 'REMOTE_HOST is required; copy isutools.mk.example to isutools.mk\n' >&2; exit 2; }
	@test -n '$(WINDOWS_USER)' || { printf 'WINDOWS_USER is required; copy isutools.mk.example to isutools.mk\n' >&2; exit 2; }

status: require-config
	@$(SSH) '/init /mnt/c/Windows/System32/cmd.exe /c "wsl.exe -l -v"'
	@$(SSH) '$(WSL) systemctl is-active nginx mysql pdns isupipe-go'

check: require-config
	@command -v ssh >/dev/null
	@command -v scp >/dev/null
	@command -v jq >/dev/null || { printf 'jq is required on the control PC\n' >&2; exit 2; }
	@$(SSH) '$(WSL) curl -fsS --max-time 10 http://127.0.0.1:$(REMOTE_ADMIN_PORT)/json >/dev/null'
	@$(SSH) '$(WSL) curl -fkSs --max-time 10 --resolve pipe.u.isucon.local:443:127.0.0.1 https://pipe.u.isucon.local/ >/dev/null'
	@$(SSH) '$(WSL) mysqladmin ping -h 127.0.0.1 -uisucon -pisucon --silent'
	@printf 'ready: ISUCON13 HTTPS, MySQL, and isutools\n'

bench: check
	@$(SSH) 'ISUTOOLS_STAGE_DIR=$(STAGE_LINUX) $(WSL) bash -s' < remote-bench.sh
	@$(MAKE) --no-print-directory pull-results
	@$(MAKE) --no-print-directory verify-results

pull-results: require-config
	@mkdir -p '$(RESULTS_DIR)'
	@$(SCP) -r '$(REMOTE_HOST):$(STAGE_LINUX)/.' '$(RESULTS_DIR)/'
	@printf 'results copied to %s\n' '$(RESULTS_DIR)'

verify-results:
	@set -eu; \
	latest=$$(find '$(RESULTS_DIR)' -maxdepth 1 -type f -name '*.json' -print | sort | while IFS= read -r candidate; do \
	  if jq -e '((.meta.schema_version | type) == "number") and ((.meta.run.run_id | type) == "string") and ((.sql | type) == "array") and ((.http | type) == "array")' "$$candidate" >/dev/null 2>&1; then \
	    printf '%s\n' "$$candidate"; \
	  fi; \
	done | tail -n 1); \
	test -n "$$latest" || { printf 'no saved snapshot JSON in %s\n' '$(RESULTS_DIR)' >&2; exit 1; }; \
	printf 'artifact: %s\n' "$$latest"; \
	jq -e '{time:.meta.time,score:.meta.score,benchmark_pass:.meta.benchmark_pass,run_id:.meta.run.run_id,validity:.meta.run.validity,sql_rows:(.sql|length),http_rows:(.http|length),accesslog_status:([.meta.health[]? | select(.collector=="accesslog")][0].status // "missing"),cpu_profile:([.meta.health[]? | select(.collector=="profile-cpu")][0].status // "missing")}' "$$latest"

pprof: check
	@set -eu; \
	case '$(PPROF_SECONDS)' in ''|*[!0-9]*) printf 'PPROF_SECONDS must be an integer\n' >&2; exit 2;; esac; \
	test '$(PPROF_SECONDS)' -ge 1 -a '$(PPROF_SECONDS)' -le 600 || { printf 'PPROF_SECONDS must be 1..600\n' >&2; exit 2; }; \
	stamp=$$(date '+%Y%m%d-%H%M%S'); \
	remote='$(STAGE_LINUX)'/cpu-$$stamp.pprof; \
	$(SSH) '$(WSL) mkdir -p $(STAGE_LINUX)'; \
	if ! $(SSH) "$(WSL) curl -fsS --max-time $$(( $(PPROF_SECONDS) + 15 )) -o '$$remote' 'http://127.0.0.1:$(REMOTE_ADMIN_PORT)/pprof/profile?seconds=$(PPROF_SECONDS)'"; then \
	  printf 'manual pprof failed; ISUTOOLS_CPU_PROFILE_MODE=run returns HTTP 409 even while idle\n' >&2; \
	  printf 'temporarily install isupipe-go.manual-pprof.conf and restart isupipe-go, or use the run profile\n' >&2; \
	  exit 1; \
	fi; \
	mkdir -p '$(RESULTS_DIR)'; \
	$(SCP) "$(REMOTE_HOST):$$remote" '$(RESULTS_DIR)/'; \
	printf 'profile copied to %s/cpu-%s.pprof\n' '$(RESULTS_DIR)' "$$stamp"

tunnel: require-config
	@set -eu; \
	guest_ip=$$($(SSH) '$(WSL) hostname -I' | awk '{print $$1}'); \
	test -n "$$guest_ip" || { printf 'could not resolve $(WSL_DISTRO) guest IP\n' >&2; exit 1; }; \
	printf 'forwarding http://127.0.0.1:$(LOCAL_PORT)/ through %s:$(GUEST_RELAY_PORT) (Ctrl-C to stop)\n' "$$guest_ip"; \
	exec ssh $(SSH_OPTIONS) -T -o ExitOnForwardFailure=yes \
	  -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
	  -L $(LOCAL_PORT):$$guest_ip:$(GUEST_RELAY_PORT) $(REMOTE_HOST) \
	  "$(WSL) bash -lc 'exec systemd-socket-activate --listen=$$guest_ip:$(GUEST_RELAY_PORT) /lib/systemd/systemd-socket-proxyd 127.0.0.1:$(REMOTE_ADMIN_PORT)'"

dashboard:
	@open 'http://127.0.0.1:$(LOCAL_PORT)/'
