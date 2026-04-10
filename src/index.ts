#!/usr/bin/env node
import { clearConfig, getConfigPath, loadConfig, saveConfig } from "./config.js";
import { post } from "./api.js";

async function fetchJson(url: string, init?: RequestInit) {
  const res = await fetch(url, init);
  const text = await res.text();
  const body = text ? JSON.parse(text) : {};
  if (!res.ok) throw new Error(body?.error ?? `Request failed (${res.status})`);
  return body;
}

function printHelp() {
  console.log(`mesh-cli v0.1.0\n
Usage:
  mesh auth login <baseUrl>
  mesh auth token-login <baseUrl> <accessToken>
  mesh auth whoami
  mesh auth logout
  mesh work-item create <title> <sourceAgentId> [scope] [projectId]
  mesh work-item update <workItemId> <agentId> [status] [currentOwner]
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
    const [baseUrl] = rest;
    if (!baseUrl) throw new Error("Usage: mesh auth login <baseUrl>");
    const data = await fetchJson(`${baseUrl.replace(/\/$/, "")}/auth/login-start`);
    console.log(JSON.stringify(data, null, 2));
    if (data?.authProvider === "supabase") {
      console.log("\nSupabase-oriented login scaffold detected.");
      if (data.supabaseUrl) console.log(`Supabase URL: ${data.supabaseUrl}`);
      if (data.supabasePublishableKey) console.log("Publishable key is available from mesh-api.");
      console.log("Temporary path: obtain a user access token and complete login with token-login.");
    }
    return;
  }

  if (group === "auth" && action === "token-login") {
    const [baseUrl, accessToken] = rest;
    if (!baseUrl || !accessToken) throw new Error("Usage: mesh auth token-login <baseUrl> <accessToken>");
    const data = await fetchJson(`${baseUrl.replace(/\/$/, "")}/auth/token-login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ accessToken }),
    });
    saveConfig({
      baseUrl,
      session: {
        kind: "user",
        baseUrl,
        accessToken,
        userEmail: data?.user?.email ?? undefined,
      },
    });
    console.log(`Saved mesh-cli session to ${getConfigPath()}`);
    console.log(JSON.stringify(data.user, null, 2));
    return;
  }

  if (group === "auth" && action === "whoami") {
    const config = loadConfig();
    if (!config?.session) throw new Error("mesh-cli is not logged in");
    console.log(JSON.stringify({
      baseUrl: config.baseUrl,
      authKind: config.session.kind,
      userEmail: config.session.userEmail ?? null,
      configured: true,
    }, null, 2));
    return;
  }

  if (group === "auth" && action === "logout") {
    clearConfig();
    console.log("Logged out of mesh-cli");
    return;
  }

  if (group === "work-item" && action === "create") {
    const [title, sourceAgentId, scope = "project", projectId] = rest;
    if (!title || !sourceAgentId) {
      throw new Error("Usage: mesh work-item create <title> <sourceAgentId> [scope] [projectId]");
    }
    const result = await post("/work-item/create", {
      title,
      sourceAgentId,
      scope,
      projectId: scope === "project" ? projectId ?? null : null,
    });
    console.log(JSON.stringify(result, null, 2));
    return;
  }

  if (group === "work-item" && action === "update") {
    const [workItemId, agentId, status, currentOwner] = rest;
    if (!workItemId || !agentId) {
      throw new Error("Usage: mesh work-item update <workItemId> <agentId> [status] [currentOwner]");
    }
    const result = await post("/work-item/update", {
      workItemId,
      agentId,
      ...(status ? { status } : {}),
      ...(currentOwner ? { currentOwner } : {}),
    });
    console.log(JSON.stringify(result, null, 2));
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
