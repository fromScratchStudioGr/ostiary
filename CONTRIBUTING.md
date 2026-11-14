# Contributing to Ostiary

Thanks for helping. This guide explains how to run Ostiary locally and how to run its tests.

## Running the environment

`docker-compose.yml` runs Postgres, Redis, and two services built from the same `Dockerfile` (through its `SERVICE` build argument): the gateway (`cmd/gateway`) and the admin API (`cmd/admin`). They run as separate processes, so a heavy admin query can never compete with the gateway's own requests for CPU or a Postgres connection. The admin panel runs alongside them.

Postgres and Redis keep their data in named volumes (`postgres_data`, `redis_data`), so users, provider keys and queued usage survive a `docker compose down` and `up`. Use `docker compose down -v` to wipe them and start from scratch.

The gateway reaches each provider at its base URL: `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, `GEMINI_BASE_URL` and `DEEPSEEK_BASE_URL`, which default to the real APIs. Point one at another server, such as a mock, to send that provider's models there.

### Start the stack

```
docker compose up --build -d
```

Check that it came up healthy:

```
docker compose ps
```

`gateway` shows `healthy` once Postgres and Redis are ready and its `/health` check passes. This can take a few seconds on the first run.

### Stop the stack

```
docker compose down
```

This keeps the volumes and their data for next time. To also wipe them and start from an empty database:

```
docker compose down -v
```

## Tests and coverage

Run all tests:

```
go test ./...
```

Add `-count=1` to bypass Go's cache of passing results, and `-run <regex>` to select tests (for example `go test ./internal/usage/ -run "^TestParseBodyUsage$" -v`).

### Tests that need Postgres and Redis

Many tests run against a real Postgres and Redis. `docker-compose.test.yml` runs them together with the whole suite, so there's nothing to start by hand:

```
docker compose -f docker-compose.test.yml run --rm tests
docker compose -f docker-compose.test.yml down
```

`run` starts Postgres and Redis, waits until they're healthy, and runs `go test ./...` in a container, writing a coverage profile (`coverage.out`) to the repo. The first run also downloads and compiles everything. The caches live in Docker volumes, so later runs start quickly. `down` removes the containers. To run a single package or test, put the command after the service name:

```
docker compose -f docker-compose.test.yml run --rm tests go test ./internal/usage/ -run "^TestPersistUsage$" -v
```

The tests find the services through `TEST_POSTGRES_ADDR` (a Postgres DSN) and `TEST_REDIS_ADDR` (`host:port`), which the `tests` service sets. A plain `go test ./...` on the host leaves them unset, so these tests are **skipped** there (as they are with `-short`), and everything else still runs. When `CI` is set (GitHub Actions sets it, and so does the `tests` service) they **fail** instead of skipping, so a missing service can't silently drop them.

To run them from the host or an IDE, start the services and point the tests at them. The compose file publishes Postgres on `127.0.0.1:55432` and Redis on `127.0.0.1:56379`:

```
docker compose -f docker-compose.test.yml up -d --wait postgres redis
export TEST_POSTGRES_ADDR="postgres://postgres:test@localhost:55432/ostiary_test?sslmode=disable"
export TEST_REDIS_ADDR="localhost:56379"
go test ./...
```

In cmd use `set NAME=value`, and in PowerShell `$env:NAME = "value"`. Each test gets its own empty Postgres schema, dropped when it ends, and unique Redis keys, so tests don't interfere with each other.

### Check coverage

For a quick figure on one package:

```
go test ./internal/usage/ -cover
```

For the whole repo, write a coverage profile and read it per function, or in the browser with covered and uncovered lines highlighted:

```
go test ./... -count=1 -coverprofile=coverage.out
go tool cover -func=coverage.out
go tool cover -html=coverage.out
```

Run on the host without the services, this skips the Postgres and Redis tests and leaves them out of the figures. The `tests` service above produces the complete `coverage.out`.

### Coverage in SonarQube (local)

`sonar-project.properties` tells the scanner to read `coverage.out`, and to leave test files, `testdata/` and the shared test helpers out of the figures.

1. Start SonarQube on a Docker network the scanner can share. The volumes keep its data between restarts. It takes a minute or two to start:

   ```
   docker network create sonar-net
   docker run -d --name sonarqube --network sonar-net -p 9000:9000 -v sonarqube_data:/opt/sonarqube/data -v sonarqube_extensions:/opt/sonarqube/extensions -v sonarqube_logs:/opt/sonarqube/logs sonarqube:community
   ```

   If the container exits right away on Windows (WSL2), raise the memory map limit it needs and start it again:

   ```
   wsl -d docker-desktop sysctl -w vm.max_map_count=262144
   ```

2. First time only: open http://localhost:9000 and sign in as `admin` / `admin` (it asks you to change the password). Create a project manually with the key `Ostiary`, choose **Locally**, and generate a token. Keep the token out of the repo.

3. Generate the coverage profile and run the scanner from the repo root. The `tests` service writes the complete `coverage.out`, including the tests that need Postgres and Redis:

   ```
   docker compose -f docker-compose.test.yml run --rm tests
   docker compose -f docker-compose.test.yml down
   docker run --rm --network sonar-net -e SONAR_HOST_URL=http://sonarqube:9000 -e SONAR_TOKEN=<your-token> -v "$(pwd):/usr/src" sonarsource/sonar-scanner-cli
   ```

   On Windows use `%cd%` (cmd) or `${PWD}` (PowerShell) in place of `$(pwd)`.

4. Read the results at http://localhost:9000/component_measures?id=Ostiary&metric=coverage.

## Key management

Provider keys are encrypted through a `KMSClient` interface with three implementations: the local KMS (the default, for development and trying Ostiary out), Google Cloud KMS and OpenBao. Choose one with `KMS_PROVIDER`. [docs/kms.md](docs/kms.md) has the full setup for each, including key rotation.
