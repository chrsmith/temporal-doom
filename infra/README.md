# Infrastructure

A Pulumi program (TypeScript) that runs DOOM-on-Temporal on AWS, in
`us-west-2`, against Temporal Cloud. It's an example of how you could host
the game yourself, at a domain of your own. This guide uses
`doom.example.com`: substitute your own, such as `doom.YOUR-DOMAIN.net`.

```
Internet → ALB (public subnets) → Fargate task: server ─┐
                                                worker ─┴→ Temporal Cloud
```

There are two Pulumi projects, one stack (`prod`) each:

- **`infra/dns`**: the Route 53 hosted zone for your domain (e.g.
  `doom.example.com`), and nothing else. It's `protect`ed and meant to live
  forever, so delegating to it from the parent domain is a one-time job.
- **`infra`**: everything else. A VPC with two public subnets and no NAT
  gateway, an ECR repository and an ARM64 image (the repo-root
  `Dockerfile`), an ACM certificate validated through the zone, an ALB
  (HTTPS, with port 80 redirecting), the `A` record pointing the domain at
  it, and one Fargate service. It can be destroyed and recreated freely.

The service runs one task with two containers from the same image: the web
server (`cmd/server`) and the worker (`cmd/worker`). They share a volume for
the leaderboard, which the worker writes and the server reads. The task runs
on Fargate Spot: if AWS reclaims it, ECS starts another, and games carry on
once it's up, since Temporal hands them to the new worker, which rebuilds
them from their histories.

The task runs in a public subnet with a public IP, so it can reach ECR,
CloudWatch and Temporal Cloud without a NAT gateway. Its security group
accepts connections only from the ALB.

## Configuration

Each stack's settings live in its `Pulumi.prod.yaml`, which `pulumi config
set` writes (encrypting secrets). These files are specific to your
deployment, so `.gitignore` keeps them out of the repo.

The `dns` stack has one setting, `domainName`. The `infra` stack's are:

| Key                 | Required    | Purpose                                                  |
| ------------------- | ----------- | -------------------------------------------------------- |
| `domainName`        | yes         | The site's domain, e.g. `doom.example.com`. Must match the `dns` stack's. |
| `temporalAddress`   | yes         | The Temporal Cloud namespace's gRPC endpoint.            |
| `temporalNamespace` | yes         | The namespace, `<namespace>.<account>`.                  |
| `temporalApiKey`    | yes, secret | A Temporal Cloud API key for the namespace.              |
| `temporalUI`        | no          | Base URL for the site's Temporal UI links, `https://cloud.temporal.io`. |
| `cpu`, `memory`     | no          | The task's size, `1024` (1 vCPU) and `2048` (MiB), shared by both containers. |
| `spot`              | no          | Run on Fargate Spot, `true`.                             |

Both containers get `TEMPORAL_ADDRESS`, `TEMPORAL_NAMESPACE` and
`TEMPORAL_API_KEY` (see `internal/setup`). The API key goes from Pulumi's
encrypted config into an SSM Parameter Store `SecureString`, and ECS injects
it into the containers when they start. It never appears in plain text in
the task definition.

## Prerequisites

- The Pulumi CLI, logged in (`pulumi login`)
- AWS credentials for the target account (`aws sts get-caller-identity`)
- Docker running, since `pulumi up` builds and pushes the image

```bash
(cd infra/dns && npm install && pulumi stack init prod)
(cd infra && npm install && pulumi stack init prod)
```

Then set your domain in both stacks:

```bash
(cd infra/dns && pulumi config set domainName doom.example.com)
(cd infra && pulumi config set domainName doom.example.com)
```

## Temporal Cloud

Point the `infra` stack at your Temporal Cloud namespace before the first
deploy:

```bash
cd infra
pulumi config set temporalAddress YOUR-NAMESPACE.YOUR-ACCOUNT.tmprl.cloud:7233
pulumi config set temporalNamespace YOUR-NAMESPACE.YOUR-ACCOUNT
pulumi config set --secret temporalApiKey <API key>
```

The key needs read and write access to the namespace. A service account's
key is better than a personal one. Temporal Cloud API keys expire: to
rotate one, set the new key and `pulumi up`, which restarts the task.

The game's custom search attributes must exist in the namespace before the
first game starts. Locally, the worker registers them itself, but on Temporal
Cloud it isn't allowed to, so add them once with `tcld` (or in the Cloud UI):

```bash
tcld namespace search-attributes add -n YOUR-NAMESPACE.YOUR-ACCOUNT \
  --sa DoomPlayer=Keyword --sa DoomMap=Keyword --sa DoomHealth=Int \
  --sa DoomKills=Int --sa DoomDeaths=Int --sa DoomMode=Keyword \
  --sa DoomPlayers=KeywordList
```

(The names are in `internal/game/types.go`.)

## Deploying

**Once: create the zone and delegate to it.**

```bash
cd infra/dns
pulumi up
pulumi stack output nameServers
```

Wherever the parent domain's DNS is hosted (`example.com`'s registrar or
DNS provider), add an `NS` record for `doom` (that is, `doom.example.com`)
for each of the four name servers. Then check that the delegation works,
which can take a few minutes, or longer if the provider is slow:

```bash
dig +short NS doom.example.com    # should list the four awsdns name servers
```

**Then: everything else.**

```bash
cd infra
pulumi up
```

This waits for ACM to issue the certificate, which needs the delegation to
be in place: run it before then, and it waits and eventually times out.

## Redeploying

`pulumi up` rebuilds the image. If it changed, ECS starts a new task, waits
for its server to pass health checks (`GET /api/config`), and stops the old
one. A task that won't come up healthy is rolled back automatically, and
`pulumi up` reports the failure. `pulumi up` builds from the working
directory, not from git, so it deploys uncommitted changes too.

**The leaderboard doesn't survive a new task.** It's on the task's own
storage, so a deploy, or Fargate Spot reclaiming the task, starts it empty.

**Take care changing the engine or workflow code.** A game only replays on
the exact `doom.wasm` that recorded it, and through workflow code that makes
the same decisions. The Dockerfile pins the Go version so that the image's
`doom.wasm` is byte-for-byte the same as a local `make engine` with the same
toolchain. Changing the engine, or Go version, strands running games.

## Debugging production

```bash
aws logs tail "$(pulumi stack output logGroupName)" --region us-west-2 --follow --since 1h

# The service's recent events: failed deployments, tasks that won't start.
aws ecs describe-services --region us-west-2 \
  --cluster "$(pulumi stack output clusterName)" \
  --services "$(pulumi stack output serviceName)" \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

If games won't start, check that the worker is polling (the `doom` task
queue's pollers, in the Cloud UI), that the search attributes exist, and
that the API key hasn't expired.

## Tearing down

```bash
cd infra && pulumi destroy
```

Leave the `dns` stack alone: destroying the zone would break the
delegation. (Keeping it costs $0.50/month.)

Approximate monthly costs: the ALB ~$16 plus usage; the task (1 vCPU,
2 GiB, ARM64) ~$29 on demand, ~$9 on Spot; public IPv4 addresses, two on
the ALB and one on the task, ~$3.60 each; and the hosted zone $0.50. About
$37 a month on Spot. Temporal Cloud is billed separately.
