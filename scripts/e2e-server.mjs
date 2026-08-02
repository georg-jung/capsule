import { spawn } from "node:child_process";

const name = "capsule-e2e-server";

function run(command, args, stdio = "inherit") {
  return new Promise(resolve => {
    const child = spawn(command, args, { stdio });
    child.on("error", () => resolve(1));
    child.on("exit", code => resolve(code ?? 1));
  });
}

await run("docker", ["rm", "--force", name], "ignore");
const build = await run("docker", ["build", "-t", "capsule:e2e", "."]);
if (build !== 0) process.exit(build);

const server = spawn("docker", [
  "run", "--rm", "--name", name,
  "-p", "127.0.0.1:18080:8080",
  "-e", "CAPSULE_ORIGIN=http://localhost:18080",
  "capsule:e2e",
], { stdio: "inherit" });

let cleaning = false;
async function cleanup() {
  if (cleaning) return;
  cleaning = true;
  await run("docker", ["rm", "--force", name], "ignore");
  process.exit(0);
}

process.on("SIGINT", cleanup);
process.on("SIGTERM", cleanup);
process.on("SIGHUP", cleanup);
server.on("exit", code => { if (!cleaning) process.exit(code ?? 0); });
