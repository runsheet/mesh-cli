#!/usr/bin/env node
import { saveConfig, loadConfig, getConfigPath } from "./config.js";
import { post } from "./api.js";

function printHelp() {
  console.log(`mesh-cli v0.1.0\n
Usage:
  mesh auth login <baseUrl> <token>
  mesh auth whoami
  mesh work-item handoff <workItemId> <fromRole> <toRole> <agentId> [summary]
  mesh work-item review <workItemId> <reviewerRole> <decision> <agentId> [notes]
  mesh work-item learning <workItemId> <agentId> [outcome]\n`);
}

async function main() {
  const [, , ...args] = process.argv;
  const [group, action, ...rest] = args;

  if (!group) {
    printHelp();
    process.exit(0);
  }

  if (group === "auth" && action === "login") {
    const [baseUrl, token] = rest;
    if (!baseUrl || !token) throw new Error("Usage: mesh auth login <baseUrl> <token>");
    saveConfig({ baseUrl, token });
    console.log(`Saved mesh-cli config to ${getConfigPath()}`);
    return;
  }

  if (group === "auth" && action === "whoami") {
    const config = loadConfig();
    if (!config) throw new Error("mesh-cli is not configured");
    console.log(JSON.stringify({ baseUrl: config.baseUrl, configured: true }, null, 2));
    return;
  }

  if (group === "work-item" && action === "handoff") {
    const [workItemId, fromRole, toRole, agentId, handoffSummary] = rest;
    if (!workItemId || !fromRole || !toRole || !agentId) {
      throw new Error("Usage: mesh work-item handoff <workItemId> <fromRole> <toRole> <agentId> [summary]");
    }
    const result = await post("/work-item/handoff", { workItemId, fromRole, toRole, agentId, handoffSummary });
    console.log(JSON.stringify(result, null, 2));
    return;
  }

  if (group === "work-item" && action === "review") {
    const [workItemId, reviewerRole, decision, agentId, notes] = rest;
    if (!workItemId || !reviewerRole || !decision || !agentId) {
      throw new Error("Usage: mesh work-item review <workItemId> <reviewerRole> <decision> <agentId> [notes]");
    }
    const result = await post("/work-item/review", { workItemId, reviewerRole, decision, agentId, notes });
    console.log(JSON.stringify(result, null, 2));
    return;
  }

  if (group === "work-item" && action === "learning") {
    const [workItemId, agentId, outcome] = rest;
    if (!workItemId || !agentId) {
      throw new Error("Usage: mesh work-item learning <workItemId> <agentId> [outcome]");
    }
    const result = await post("/work-item/learning", { workItemId, agentId, outcome });
    console.log(JSON.stringify(result, null, 2));
    return;
  }

  printHelp();
}

main().catch((err) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
