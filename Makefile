SERVICES := api scheduler worker alerting notifier

.PHONY: build
build:
	@for s in $(SERVICES); do \
		echo "building $$s"; \
		go build -o bin/$$s ./cmd/$$s || exit 1; \
	done

.PHONY: test
test:
	go test ./...

.PHONY: test-integration
test-integration:
	go test -tags integration -count=1 -timeout 300s ./test/integration/

.PHONY: lint
lint:
	gofmt -l .
	go vet ./...

# Bootstrap a brand-new (empty) database with the baseline + migrations. Refuses to
# run against an already-initialized database, so it can't wipe data. Day-to-day
# schema changes go through `make migrate`, never by re-running this.
.PHONY: schema
schema:
	go run ./cmd/schema

# Apply pending migrations to PULSE_POSTGRES_DSN (forward-only, never drops data).
# This is the normal way to change the schema of any real database.
.PHONY: migrate
migrate:
	go run ./cmd/migrate

# Create a new timestamped migration: make migrate-create name=add_widget_table
.PHONY: migrate-create
migrate-create:
	@test -n "$(name)" || (echo "usage: make migrate-create name=<snake_case_name>" && exit 1)
	go tool goose -dir internal/store/migrations create $(name) sql

.PHONY: reset
reset:
	docker compose down -v
	docker compose up -d

.PHONY: up
up:
	docker compose up -d

.PHONY: down
down:
	docker compose down -v

.PHONY: tidy
tidy:
	go mod tidy

# Regenerate the API contract artifacts from api/openapi/v1.yaml (RFC-012):
# Go server types/stubs and TS client types.
.PHONY: gen
gen:
	go tool oapi-codegen -config api/openapi/codegen.yaml api/openapi/v1.yaml
	cd web && npm run gen:api

# Assemble the static docs site (GitHub Pages). Copies api/openapi/v1.yaml ->
# docs-site/openapi.yaml so the API reference (Redoc) cannot drift from the spec.
# Reproducible and offline; the Redoc CDN script is only used in the browser.
.PHONY: docs
docs:
	./docs-site/build.sh

# Drift check (RFC-012 8.3): the spec must lint, and the committed generated
# files must match a fresh regeneration. Fails the build if the spec and code
# disagree. Run in CI.
.PHONY: gen-check
gen-check: gen
	cd web && npm run lint:api
	git diff --exit-code -- internal/apigen/apigen.gen.go web/src/api/schema.d.ts
