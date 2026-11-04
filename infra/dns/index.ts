/**
 * DOOM on Temporal: the Route 53 hosted zone for the site's domain, e.g.
 * doom.example.com.
 *
 * The parent domain's DNS (example.com's) stays where it is, and delegates
 * just this subdomain here with NS records (the `nameServers` output), added
 * once.
 *
 * The zone lives in its own stack, protected, so the main stack (../) can be
 * destroyed and recreated freely: a new zone would get new name servers,
 * and the delegation would have to be redone.
 */

import * as aws from "@pulumi/aws";
import * as pulumi from "@pulumi/pulumi";

// The site's domain, e.g. doom.example.com:
//   pulumi config set domainName doom.example.com
const domainName = new pulumi.Config().require("domainName");

const zone = new aws.route53.Zone(
    "doom",
    { name: domainName, comment: "DOOM on Temporal (temporal-doom infra/dns)" },
    { protect: true },
);

export const zoneId = zone.zoneId;
export const zoneName = zone.name;
// Add these as NS records for the subdomain wherever the parent domain's DNS
// is hosted (e.g. an NS record for `doom` in example.com's zone).
export const nameServers = zone.nameServers;
