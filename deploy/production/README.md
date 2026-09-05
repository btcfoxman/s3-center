# Production deployment

The stack runs the application, PostgreSQL, and Cloudflare Tunnel. Only
`127.0.0.1:48080` is published on the host; public traffic enters through the
tunnel.

Copy `.env.example` to `.env` on the server and replace every value. Image
selection is intentionally not stored in `.env`; the workflow passes
`IMAGE_REGISTRY`, `IMAGE_NAMESPACE`, `IMAGE_NAME`, and `IMAGE_TAG` to Compose.
After a successful deployment it also writes those non-secret values to the
separate `.image.env` operator file and installs `s3-centerctl`. The application
secret file `.env` is never overwritten by the control script.

Manual service control on the server does not contact GHCR and does not require
a registry login:

```bash
cd /path/to/s3-center
./s3-centerctl stop       # stop the public app/tunnel; keep PostgreSQL running
./s3-centerctl start      # start the existing app/tunnel and wait for readiness
./s3-centerctl restart    # restart only the app and wait for readiness
./s3-centerctl recreate   # recreate the app from the cached deployed image
./s3-centerctl status
./s3-centerctl health
./s3-centerctl logs       # Ctrl-C exits log following
```

Use `stop-all` and `start-all` only when PostgreSQL must also be stopped. These
commands preserve containers, the PostgreSQL volume, configuration, and all
dynamic backend records. Image updates remain the responsibility of the
`Production Docker` workflow; the manual commands deliberately never pull an
uncached image.

Operator guides:

- [Dynamic Cloudflare R2 account onboarding](R2-ACCOUNT-MANAGEMENT.md)
- [Index-mode third-party integration](INDEX-MODE-API.md)
- [OpenAPI 3.1 contract](index-mode-openapi.yaml)

Required GitHub secrets: `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY`,
`DEPLOY_PORT`, `DEPLOY_PATH`, `DEPLOY_SERVICE`, `GHCR_USERNAME`, and
`GHCR_TOKEN`. Cloudflare Tunnel/DNS automation also uses `CF_ACCOUNT_ID` and
`CF_API_TOKEN`. The isolated browser onboarding connector does not require an
OAuth client or Cloudflare Access. Set repository variable
`REGISTRY_TARGET=ghcr`.

The production workflow enables the connector after deploying the application.
Each administrator-created bootstrap token is submitted over a single-use HTTPS
channel, held in memory, used through the account's selected proxy, and revoked
after the bucket-scoped R2 credential has been verified and encrypted.
