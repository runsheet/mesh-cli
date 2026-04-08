import fs from "node:fs";
import path from "node:path";
import os from "node:os";

export interface MeshCliConfig {
  baseUrl: string;
  token: string;
}

const CONFIG_PATH = path.join(os.homedir(), ".mesh-cli.json");

export function getConfigPath() {
  return CONFIG_PATH;
}

export function loadConfig(): MeshCliConfig | null {
  const envBaseUrl = process.env.MESH_API_BASE_URL;
  const envToken = process.env.MESH_MACHINE_TOKEN;
  if (envBaseUrl && envToken) {
    return { baseUrl: envBaseUrl, token: envToken };
  }

  if (!fs.existsSync(CONFIG_PATH)) return null;
  const raw = fs.readFileSync(CONFIG_PATH, "utf8");
  return JSON.parse(raw) as MeshCliConfig;
}

export function saveConfig(config: MeshCliConfig) {
  fs.writeFileSync(CONFIG_PATH, JSON.stringify(config, null, 2));
}
