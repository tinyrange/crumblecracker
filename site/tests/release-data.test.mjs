import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { copyFile, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import test from "node:test";

const exec = promisify(execFile);

for (const includeShell of [false, true]) {
  test(`release refresh accepts desktop releases${includeShell ? " and ignores legacy shell assets" : " without a shell asset"}`, async (t) => {
    const root = await mkdtemp(join(tmpdir(), "desktop-release-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    await mkdir(join(root, "scripts"));
    await mkdir(join(root, "app"));
    const script = join(root, "scripts/update-release.mjs");
    await copyFile(new URL("../scripts/update-release.mjs", import.meta.url), script);
    const products = ["NeurodeskAppX", "SquadVM", ...(includeShell ? ["vmsh"] : [])];
    const latest = {
      tag_name: "v1.0.0",
      html_url: "https://github.com/tinyrange/vmsh/releases/tag/v1.0.0",
      assets: products.map((product) => ({
        name: `${product}_v1.0.0_linux_amd64`,
        size: 1024,
        browser_download_url: `https://github.com/tinyrange/vmsh/releases/download/v1.0.0/${product}_v1.0.0_linux_amd64`,
      })),
    };
    const preload = `globalThis.fetch = async () => Response.json(${JSON.stringify(latest)});`;
    await exec(process.execPath, ["--import", `data:text/javascript,${encodeURIComponent(preload)}`, script]);
    const actual = JSON.parse(await readFile(join(root, "app/release-data.json"), "utf8"));
    assert.deepEqual(actual.assets.map((asset) => asset.product), ["NeurodeskAppX", "SquadVM"]);
    assert.equal(actual.tag, latest.tag_name);
    assert.equal(actual.checksums, latest.html_url);
  });
}
