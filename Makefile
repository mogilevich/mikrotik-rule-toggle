HOST_IP ?= $(shell ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($$i=="src") {print $$(i+1); exit}}')
ifeq ($(HOST_IP),)
HOST_IP := $(shell ipconfig getifaddr en0 2>/dev/null)
endif

up:
	HOST_IP=$(HOST_IP) docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

pull:
	docker compose pull

restart: pull
	HOST_IP=$(HOST_IP) docker compose up -d --force-recreate

build-local:
	docker build -t ghcr.io/mogilevich/mikrotik-rule-toggle:latest .
	HOST_IP=$(HOST_IP) docker compose up -d --force-recreate
