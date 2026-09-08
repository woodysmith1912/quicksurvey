BINARY := quicksurvey
DATA   ?= ./data

# no-new-privileges is off by default because it is incompatible with
# snap-packaged Docker; see the comment in compose.yaml. Set QS_NNP=1 to add it
# on a host where it works.
COMPOSE := docker compose -f compose.yaml
ifdef QS_NNP
COMPOSE += -f compose.no-new-privileges.yaml
endif

.PHONY: build test vet fmt run docker up down e2e e2e-docker e2e-install clean check

build:
	go build -trimpath -o $(BINARY) ./cmd/quicksurvey

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test

# Run locally on plain HTTP. Secure cookies are off because a browser drops
# them over http://localhost.
run: build
	QS_DATA_DIR=$(DATA) ./$(BINARY) serve -addr :8080 -secure-cookies=false

docker:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

# Browser tests, entirely in containers: nothing is installed on the host.
# The test volume is removed first so the admin-seeding container starts clean.
e2e-docker:
	$(COMPOSE) --profile test down -v --remove-orphans
	DOCKER_UID=$$(id -u) DOCKER_GID=$$(id -g) \
	  $(COMPOSE) --profile test run --rm --build playwright
	$(COMPOSE) --profile test down -v --remove-orphans

# The same tests against a locally built binary, for a faster edit loop. This
# one does install Playwright and a browser on the host.
e2e-install:
	cd e2e && npm install && npx playwright install --with-deps chromium

e2e: build
	cd e2e && npx playwright test

clean:
	rm -f $(BINARY)
	rm -rf e2e/test-results e2e/playwright-report
