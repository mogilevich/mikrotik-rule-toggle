HOST_IP ?= $(shell (ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/{print $$NF; exit}') || true)
ifeq ($(HOST_IP),)
HOST_IP := $(shell ipconfig getifaddr en0 2>/dev/null)
endif

up:
	HOST_IP=$(HOST_IP) docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f

restart:
	HOST_IP=$(HOST_IP) docker compose up --build -d --force-recreate
