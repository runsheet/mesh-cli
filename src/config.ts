import fs from "node:fs";
import path from "node:path";
import os from "node:os";

export interface MeshCliSession {
  kind: "user";
  baseUrl: string;
  accessToken: string;
  refreshToken?: string;
  userEmail?: string;
}

export interface MeshCliConfig {
  baseUrl: string;
  session?: MeshCliSession;
}

const CONFIG_PATH = path.join(os.homedir(), ".mesh-cli.json");

export function getConfigPath() {
  return CONFIG_PATH;
}

export function loadConfig(): MeshCliConfig | null {
  if (!fs.existsSync(CONFIG_PATH)) return null;
  const raw = fs.readFileSync(CONFIG_PATH, "utf8");
  return JSON.parse(raw) as MeshCliConfig;
}

export function saveConfig(config: MeshCliConfig) {
  fs.writeFileSync(CONFIG_PATH, JSON.stringify(config, null, 2));
}

export function clearConfig() {
  if (fs.existsSync(CONFIG_PATH)) fs.unlinkSync(CONFIG_PATH);
}
