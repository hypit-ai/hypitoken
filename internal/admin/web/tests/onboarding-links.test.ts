import assert from "node:assert/strict";
import { test } from "node:test";
import { buildCCSwitchURL, claudeCodeConfig, claudeCodeEnv } from "../src/lib/onboarding-links";

const config = { token: "sk-test-only", baseUrl: "https://gateway.example/", providerName: "Test Gateway" };

// Contract from QuantumNous/new-api ba5f73677, cc-switch-dialog.tsx.
// Compare the complete serialized protocol, including omitted extension fields.
for (const app of ["codex", "claude"] as const) {
  test(`${app} import matches New API wire format`, () => {
    const endpoint = app === "codex" ? "https://gateway.example/v1" : "https://gateway.example";
    const models = app === "codex"
      ? { model: "gpt-6.1-sol" }
      : { model: "claude-sonnet-5", haikuModel: "claude-haiku-4-5", sonnetModel: "claude-sonnet-5", opusModel: "claude-opus-5-5" };
    const expected = new URLSearchParams({ resource: "provider", app, name: config.providerName, endpoint, apiKey: config.token });
    for (const [key, value] of Object.entries(models)) expected.set(key, value);
    expected.set("homepage", "https://gateway.example");
    expected.set("enabled", "true");
    assert.equal(buildCCSwitchURL(config, app, models), `ccswitch://v1/import?${expected}`);
  });
}

test("model and credential characters round-trip without duplicate encoding", () => {
  const token = "sk-test+only/&=中文";
  const model = "vendor/model+custom&value=1";
  const p = new URL(buildCCSwitchURL({ ...config, token, providerName: "测试服务商" }, "codex", { model })).searchParams;
  assert.equal(p.get("apiKey"), token);
  assert.equal(p.get("name"), "测试服务商");
  assert.equal(p.get("model"), model);
  for (const key of ["icon", "config", "fableModel", "haikuModel", "sonnetModel", "opusModel"]) assert.equal(p.has(key), false);
});

test("empty optional model fields are omitted as in New API", () => {
  const p = new URL(buildCCSwitchURL(config, "claude", { model: "", haikuModel: "", sonnetModel: "", opusModel: "" })).searchParams;
  for (const key of ["model", "haikuModel", "sonnetModel", "opusModel", "config", "icon"]) assert.equal(p.has(key), false);
});

for (const os of ["macOS", "Linux", "Windows"] as const) {
  test(`${os} Claude shell setup selects the real local classifier without bypassing permissions`, () => {
    const prefix = os === "Windows" ? "$env:" : "export ";
    assert.equal(
      claudeCodeEnv(config, os),
      [
        `${prefix}ANTHROPIC_BASE_URL="https://gateway.example"`,
        `${prefix}ANTHROPIC_AUTH_TOKEN="${config.token}"`,
        `${prefix}CLAUDE_CODE_AUTO_MODE_SERVER="0"`,
      ].join("\n"),
    );
  });

  test(`${os} Claude settings preserve auth and only configure local auto-mode classification`, () => {
    const snippet = claudeCodeConfig(config, os);
    const settings = JSON.parse(snippet.slice(snippet.indexOf("{"), snippet.lastIndexOf("}") + 1));
    assert.deepEqual(settings, {
      env: {
        ANTHROPIC_BASE_URL: "https://gateway.example",
        ANTHROPIC_AUTH_TOKEN: config.token,
        CLAUDE_CODE_AUTO_MODE_SERVER: "0",
      },
    });
  });
}

