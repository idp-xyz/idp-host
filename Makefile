# idp-host build and test entry points.
PG_IMAGE ?= public.ecr.aws/docker/library/postgres:17-alpine
PG_NAME  ?= idp-host-test-pg

.PHONY: build test vet test-integration pg-up pg-down

build:
	go build -o bin/ ./cmd/...

vet:
	gofmt -l . | (! grep .) && go vet ./...

test: vet
	go test -race ./...

# Starts a throwaway PostgreSQL on a random loopback port, runs the integration tests against it and
# removes it again.
test-integration: pg-up
	@PORT=$$(docker port $(PG_NAME) 5432/tcp | head -1 | sed 's/.*://'); \
	IDP_TEST_PG_OWNER_URL="postgres://postgres:test@127.0.0.1:$$PORT/postgres?sslmode=disable" \
	go test -count=1 -race -v -timeout 10m ./tests/integration/ ; status=$$?; \
	$(MAKE) --no-print-directory pg-down; exit $$status

pg-up:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(PG_NAME) -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 60); do docker exec $(PG_NAME) pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break; sleep 0.5; done

pg-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true

# Live model gate: the real endpoints of the operator (see tests/live). Example:
#   IDP_LIVE_OPENAI_URL=http://127.0.0.1:8080/v1 IDP_LIVE_ANTHROPIC_URL=http://127.0.0.1:8080 IDP_LIVE_MODEL=qwen3.8-flash-next-iq3_xxs make test-live
.PHONY: test-live
test-live:
	go test -count=1 -v -timeout 10m ./tests/live/
