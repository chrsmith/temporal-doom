/**
 * DOOM on Temporal: an example of hosting the web server and worker on AWS
 * ECS Fargate, at a domain of your own (e.g. https://doom.example.com).
 *
 *   Internet → ALB (public subnets) → Fargate task: server ─┐
 *                                                   worker ─┴→ Temporal Cloud
 *
 * One service runs one task with two containers from the same image (the
 * repo-root Dockerfile): the web server (cmd/server) and the worker
 * (cmd/worker). They share a volume for the leaderboard file, which the
 * worker writes and the server reads.
 *
 * The hosted zone comes from the separate `dns` stack (./dns), so this stack
 * can be destroyed and recreated without touching the registrar.
 *
 * Cost-conscious defaults:
 *   - ARM64 / Graviton Fargate (~20% cheaper), on Fargate Spot (~70% off).
 *     A game survives its worker being reclaimed: Temporal hands it to the
 *     replacement, which rebuilds it by replaying history.
 *   - No NAT gateway: the task runs in public subnets with a public IP (for
 *     pulling its image, writing logs and reaching Temporal Cloud), but its
 *     security group admits traffic only from the ALB.
 *   - The Temporal API key is an SSM SecureString (free), not a Secrets
 *     Manager secret ($0.40/month).
 */

import * as pulumi from "@pulumi/pulumi";
import * as aws from "@pulumi/aws";
import * as awsx from "@pulumi/awsx";

const config = new pulumi.Config();
// The site's domain, e.g. doom.example.com. Must match the dns stack's zone.
const domainName = config.require("domainName");
const containerPort = 8080;
// The task's size, shared by both containers. The worker runs a DOOM engine
// (under wazero) per active game, so it gets most of it.
const cpu = config.get("cpu") ?? "1024";
const memory = config.get("memory") ?? "2048";
const spot = config.getBoolean("spot") ?? true;

// Temporal Cloud. Required:
//   pulumi config set domainName doom.example.com
//   pulumi config set temporalAddress <namespace gRPC endpoint, host:port>
//   pulumi config set temporalNamespace <namespace>.<account>
//   pulumi config set --secret temporalApiKey <API key>
const temporalAddress = config.require("temporalAddress");
const temporalNamespace = config.require("temporalNamespace");
const temporalApiKey = config.requireSecret("temporalApiKey");
// Where the site's "view in Temporal" links point.
const temporalUI = config.get("temporalUI") ?? "https://cloud.temporal.io";

// ---------------------------------------------------------------------------
// Networking: public subnets only, across two AZs (an ALB requires two).
// ---------------------------------------------------------------------------
const vpc = new awsx.ec2.Vpc("doom", {
    numberOfAvailabilityZones: 2,
    natGateways: { strategy: "None" },
    subnetStrategy: "Auto",
    subnetSpecs: [{ type: "Public", cidrMask: 20 }],
});

// ---------------------------------------------------------------------------
// Security groups: ALB ← internet, task ← ALB only
// ---------------------------------------------------------------------------
const albSg = new aws.ec2.SecurityGroup("doom-alb", {
    vpcId: vpc.vpcId,
    description: "DOOM ALB: public HTTP(S) ingress",
    ingress: [
        { protocol: "tcp", fromPort: 80, toPort: 80, cidrBlocks: ["0.0.0.0/0"] },
        { protocol: "tcp", fromPort: 443, toPort: 443, cidrBlocks: ["0.0.0.0/0"] },
    ],
    egress: [{ protocol: "-1", fromPort: 0, toPort: 0, cidrBlocks: ["0.0.0.0/0"] }],
});

const taskSg = new aws.ec2.SecurityGroup("doom-task", {
    vpcId: vpc.vpcId,
    description: "DOOM Fargate task: only the ALB may reach the web server",
    egress: [{ protocol: "-1", fromPort: 0, toPort: 0, cidrBlocks: ["0.0.0.0/0"] }],
});
new aws.ec2.SecurityGroupRule("doom-task-from-alb", {
    type: "ingress",
    securityGroupId: taskSg.id,
    protocol: "tcp",
    fromPort: containerPort,
    toPort: containerPort,
    sourceSecurityGroupId: albSg.id,
});

// ---------------------------------------------------------------------------
// Container image: built locally for linux/arm64 and pushed to a private ECR
// repository. Both containers run it.
// ---------------------------------------------------------------------------
const repo = new awsx.ecr.Repository("doom", { forceDelete: true });
const image = new awsx.ecr.Image("doom", {
    repositoryUrl: repo.url,
    context: "..", // repo root, where the Dockerfile and go module live
    dockerfile: "../Dockerfile",
    platform: "linux/arm64",
});

// ---------------------------------------------------------------------------
// DNS and TLS. The zone is the `dns` stack's; once the registrar delegates to
// it, ACM validates the certificate through it with no manual steps.
// ---------------------------------------------------------------------------
const zone = aws.route53.getZoneOutput({ name: domainName });

const certificate = new aws.acm.Certificate("doom", {
    domainName: domainName,
    validationMethod: "DNS",
});
const validationRecords = certificate.domainValidationOptions.apply((opts) =>
    opts.map(
        (o, i) =>
            new aws.route53.Record(`doom-cert-validation-${i}`, {
                zoneId: zone.zoneId,
                name: o.resourceRecordName,
                type: o.resourceRecordType,
                records: [o.resourceRecordValue],
                ttl: 300,
                allowOverwrite: true,
            }),
    ),
);
// Waits for ACM to issue the certificate, so the HTTPS listener can use it.
const validation = new aws.acm.CertificateValidation("doom", {
    certificateArn: certificate.arn,
    validationRecordFqdns: validationRecords.apply((rs) => rs.map((r) => r.fqdn)),
});

// ---------------------------------------------------------------------------
// Load balancer: HTTPS on 443, and port 80 redirects to it.
// ---------------------------------------------------------------------------
const alb = new aws.lb.LoadBalancer("doom", {
    loadBalancerType: "application",
    internal: false,
    securityGroups: [albSg.id],
    subnets: vpc.publicSubnetIds,
});

const targetGroup = new aws.lb.TargetGroup("doom", {
    port: containerPort,
    protocol: "HTTP",
    targetType: "ip", // required for Fargate (awsvpc networking)
    vpcId: vpc.vpcId,
    deregistrationDelay: 15,
    healthCheck: {
        // Served from memory, without calling Temporal, so a Temporal Cloud
        // outage doesn't take the site's tasks out of service.
        path: "/api/config",
        protocol: "HTTP",
        matcher: "200",
        interval: 30,
        timeout: 5,
        healthyThreshold: 2,
        unhealthyThreshold: 3,
    },
});

new aws.lb.Listener("doom-http", {
    loadBalancerArn: alb.arn,
    port: 80,
    protocol: "HTTP",
    defaultActions: [
        { type: "redirect", redirect: { port: "443", protocol: "HTTPS", statusCode: "HTTP_301" } },
    ],
});

new aws.lb.Listener("doom-https", {
    loadBalancerArn: alb.arn,
    port: 443,
    protocol: "HTTPS",
    sslPolicy: "ELBSecurityPolicy-TLS13-1-2-2021-06",
    certificateArn: validation.certificateArn,
    defaultActions: [{ type: "forward", targetGroupArn: targetGroup.arn }],
});

new aws.route53.Record("doom", {
    zoneId: zone.zoneId,
    name: domainName,
    type: "A",
    aliases: [{ name: alb.dnsName, zoneId: alb.zoneId, evaluateTargetHealth: true }],
});

// ---------------------------------------------------------------------------
// The Temporal API key, injected into the containers as an ECS secret (never
// plaintext in the task definition). It uses the account's default aws/ssm
// KMS key, which the execution role can decrypt with no extra grant.
// ---------------------------------------------------------------------------
const temporalApiKeyParam = new aws.ssm.Parameter("doom-temporal-api-key", {
    type: "SecureString",
    value: temporalApiKey,
    description: "Temporal Cloud API key for DOOM on Temporal",
});

// ---------------------------------------------------------------------------
// IAM: the execution role pulls the image, writes logs, and reads the API
// key. Neither command calls AWS APIs, so awsx's default task role carries
// no permissions.
// ---------------------------------------------------------------------------
const executionRole = new aws.iam.Role("doom-exec", {
    assumeRolePolicy: JSON.stringify({
        Version: "2012-10-17",
        Statement: [
            { Action: "sts:AssumeRole", Effect: "Allow", Principal: { Service: "ecs-tasks.amazonaws.com" } },
        ],
    }),
});
new aws.iam.RolePolicyAttachment("doom-exec-managed", {
    role: executionRole.name,
    policyArn: "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy",
});
const secretsPolicy = new aws.iam.RolePolicy("doom-exec-secrets", {
    role: executionRole.id,
    policy: temporalApiKeyParam.arn.apply((arn) =>
        JSON.stringify({
            Version: "2012-10-17",
            Statement: [{ Effect: "Allow", Action: ["ssm:GetParameters"], Resource: [arn] }],
        }),
    ),
});

// ---------------------------------------------------------------------------
// ECS cluster + Fargate service, logging to CloudWatch.
// ---------------------------------------------------------------------------
const logGroup = new aws.cloudwatch.LogGroup("doom", { retentionInDays: 14 });
const cluster = new aws.ecs.Cluster("doom", {
    settings: [{ name: "containerInsights", value: "disabled" }], // keep CloudWatch costs minimal
});
const capacityProviders = new aws.ecs.ClusterCapacityProviders("doom", {
    clusterName: cluster.name,
    capacityProviders: ["FARGATE", "FARGATE_SPOT"],
});

// How both commands reach Temporal (see internal/setup).
const temporalEnvironment = [
    { name: "TEMPORAL_ADDRESS", value: temporalAddress },
    { name: "TEMPORAL_NAMESPACE", value: temporalNamespace },
];
const temporalSecrets = [{ name: "TEMPORAL_API_KEY", valueFrom: temporalApiKeyParam.arn }];
// The leaderboard file, at the commands' default path (data/leaderboard.jsonl,
// relative to the image's /app). The path must match the Dockerfile's VOLUME,
// so the volume is writable by the image's nonroot user.
const dataMount = [{ sourceVolume: "data", containerPath: "/app/data" }];

const service = new awsx.ecs.FargateService(
    "doom",
    {
        cluster: cluster.arn,
        // One task: the leaderboard lives on its volume, so a second task
        // would keep a second leaderboard.
        desiredCount: 1,
        capacityProviderStrategies: [{ capacityProvider: spot ? "FARGATE_SPOT" : "FARGATE", weight: 1 }],
        // Changing the capacity provider strategy requires a new deployment.
        forceNewDeployment: true,
        networkConfiguration: {
            subnets: vpc.publicSubnetIds,
            assignPublicIp: true, // no NAT, so this is how the task reaches the internet
            securityGroups: [taskSg.id],
        },
        // On a bad deploy, let ECS detect the failed rollout and roll back to the
        // last good task definition instead of crash-looping the new one forever.
        // awsx waits for steady state, so `pulumi up` surfaces the failure; the
        // 10m customTimeout below bounds how long it waits.
        deploymentCircuitBreaker: { enable: true, rollback: true },
        taskDefinitionArgs: {
            runtimePlatform: { cpuArchitecture: "ARM64", operatingSystemFamily: "LINUX" },
            cpu: cpu,
            memory: memory,
            executionRole: { roleArn: executionRole.arn },
            logGroup: { existing: { name: logGroup.name } },
            volumes: [{ name: "data" }],
            containers: {
                server: {
                    name: "server",
                    image: image.imageUri,
                    essential: true,
                    entryPoint: ["/app/server"],
                    command: ["-listen", `:${containerPort}`, "-temporal-ui", temporalUI],
                    portMappings: [{ containerPort: containerPort, targetGroup: targetGroup }],
                    mountPoints: dataMount,
                    environment: temporalEnvironment,
                    secrets: temporalSecrets,
                },
                worker: {
                    name: "worker",
                    image: image.imageUri,
                    essential: true,
                    entryPoint: ["/app/worker"],
                    mountPoints: dataMount,
                    environment: temporalEnvironment,
                    secrets: temporalSecrets,
                },
            },
        },
    },
    { dependsOn: [secretsPolicy, capacityProviders], customTimeouts: { create: "10m", update: "10m" } },
);

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------
export const appUrl = `https://${domainName}`;
export const albDnsName = alb.dnsName;
export const imageUri = image.imageUri;
export const clusterName = cluster.name;
export const serviceName = service.service.name;
export const logGroupName = logGroup.name;
export const temporal = pulumi.interpolate`${temporalAddress}/${temporalNamespace}`;
