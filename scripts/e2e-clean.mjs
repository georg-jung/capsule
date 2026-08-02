import { spawn } from "node:child_process";

const cleanup = spawn("docker", ["rm", "--force", "capsule-e2e-server"], {
  stdio: "ignore",
});

cleanup.on("error", () => process.exit(0));
cleanup.on("exit", () => process.exit(0));
