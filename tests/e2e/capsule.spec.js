const { test, expect } = require("@playwright/test");

test("complete private app lifecycle works online and offline", async ({ browser }) => {
  const ownerContext = await browser.newContext();
  await ownerContext.credentials.install();
  const ownerPage = await ownerContext.newPage();

  await ownerPage.goto("/");
  await expect(ownerPage.getByRole("heading", { name: "Set up your private app" })).toBeVisible();
  await ownerPage.getByLabel("App name").fill("Pocket tools");
  await ownerPage.getByLabel("Your name").fill("Georg");
  await ownerPage.getByRole("button", { name: "Create owner passkey" }).click();
  await expect(ownerPage).toHaveURL(/\/app$/);
  await expect(ownerPage.getByRole("heading", { name: "Pocket tools" })).toBeVisible();

  await ownerPage.locator("#file-input").setInputFiles([
    {
      name: "alpha.html",
      mimeType: "text/html",
      buffer: Buffer.from("<!doctype html><h1>Alpha</h1><script>localStorage.setItem('capsule-e2e','shared')</script>"),
    },
    {
      name: "beta.html",
      mimeType: "text/html",
      buffer: Buffer.from("<!doctype html><h1>Beta</h1><output id='stored'></output><script>stored.textContent=localStorage.getItem('capsule-e2e')||'missing'</script>"),
    },
  ]);
  await expect(ownerPage.locator("#file-summary")).toHaveText("2 files");

  const transfer = await ownerPage.evaluateHandle(() => {
    const value = new DataTransfer();
    value.items.add(new File(["<!doctype html><h1>Dragged</h1>"], "dragged.html", { type: "text/html" }));
    return value;
  });
  await ownerPage.locator("#drop-zone").dispatchEvent("drop", { dataTransfer: transfer });
  await expect(ownerPage.locator("#file-summary")).toHaveText("3 files");

  const alphaLink = ownerPage.getByRole("link", { name: /alpha\.html/i });
  const originalAlphaURL = await alphaLink.getAttribute("href");
  await ownerPage.locator("#file-input").setInputFiles({
    name: "alpha.html",
    mimeType: "text/html",
    buffer: Buffer.from("<!doctype html><h1>Alpha v2</h1><script>localStorage.setItem('capsule-e2e','shared')</script>"),
  });
  await expect(ownerPage.locator("#toast")).toContainText("1 replaced");
  await expect(alphaLink).not.toHaveAttribute("href", originalAlphaURL);
  const updatedAlphaURL = await alphaLink.getAttribute("href");
  await alphaLink.click();
  await expect(ownerPage.getByRole("heading", { name: "Alpha v2" })).toBeVisible();
  await ownerPage.goBack();
  await ownerPage.getByRole("link", { name: /beta\.html/i }).click();
  await expect(ownerPage.locator("#stored")).toHaveText("shared");
  await ownerPage.goBack();

  const manifest = await ownerPage.request.get("/manifest.webmanifest");
  expect(manifest.ok()).toBeTruthy();
  expect(manifest.headers()["content-type"]).toContain("application/manifest+json");
  expect((await manifest.json()).name).toBe("Pocket tools");
  await expect(ownerPage.locator('link[rel="manifest"]')).toHaveAttribute("crossorigin", "use-credentials");

  await ownerPage.getByRole("button", { name: "Manage" }).click();
  await expect(ownerPage.locator("#owner-list")).toContainText("Georg");
  await ownerPage.getByRole("button", { name: "Invite owner" }).click();
  await expect(ownerPage.locator("#invite-result")).toContainText("/join#");
  const inviteText = await ownerPage.locator("#invite-result div").textContent();
  const inviteURL = inviteText.match(/http:\/\/localhost:18080\/join#[A-Za-z0-9_-]+/)[0];

  const secondContext = await browser.newContext();
  await secondContext.credentials.install();
  const secondPage = await secondContext.newPage();
  await secondPage.goto(inviteURL);
  await expect(secondPage).toHaveURL("/join");
  await secondPage.getByLabel("Your name").fill("Peter");
  await secondPage.getByRole("button", { name: "Register owner passkey" }).click();
  await expect(secondPage).toHaveURL(/\/app$/);
  await secondPage.getByRole("button", { name: "Manage" }).click();
  await expect(secondPage.locator("#owner-list")).toContainText("Georg");
  await expect(secondPage.locator("#owner-list")).toContainText("Peter");

  await secondPage.getByRole("button", { name: "Invite owner" }).click();
  await expect(secondPage.locator("#invite-result")).toContainText("/join#");
  const blockedInviteText = await secondPage.locator("#invite-result div").textContent();
  const blockedInviteURL = blockedInviteText.match(/http:\/\/localhost:18080\/join#[A-Za-z0-9_-]+/)[0];
  const blockedContext = await browser.newContext({ serviceWorkers: "block" });
  await blockedContext.credentials.install();
  const blockedPage = await blockedContext.newPage();
  await blockedPage.goto(blockedInviteURL);
  await blockedPage.getByLabel("Your name").fill("Ada");
  await blockedPage.getByRole("button", { name: "Register owner passkey" }).click();
  await expect(blockedPage).toHaveURL(/\/app$/);
  await blockedPage.getByRole("button", { name: "Manage" }).click();
  await blockedPage.getByRole("button", { name: "Log out on this device" }).click();
  await expect(blockedPage).toHaveURL("/");
  await blockedContext.close();

  const georgCard = secondPage.locator(".owner-card", { hasText: "Georg" });
  secondPage.once("dialog", dialog => dialog.accept());
  await georgCard.getByRole("button", { name: "Delete owner" }).click();
  await expect(secondPage.locator("#owner-list")).not.toContainText("Georg");
  await ownerPage.goto(updatedAlphaURL);
  await expect(ownerPage.getByRole("heading", { name: "Alpha v2" })).not.toBeVisible();
  await ownerPage.goto("/app");
  await expect(ownerPage).toHaveURL("/");
  await ownerContext.close();

  await secondPage.locator("#settings-dialog").getByRole("button", { name: "Close" }).click();
  await expect(secondPage.locator("#offline-status")).toHaveText("Ready offline", { timeout: 30_000 });
  await secondContext.setOffline(true);
  await secondPage.reload();
  await expect(secondPage.locator("#file-summary")).toHaveText("3 files");
  await expect(secondPage.getByRole("button", { name: "Choose files" })).toBeDisabled();
  await secondPage.getByRole("link", { name: /alpha\.html/i }).click();
  await expect(secondPage.getByRole("heading", { name: "Alpha v2" })).toBeVisible();
  await secondPage.goBack();
  await secondPage.getByRole("link", { name: /beta\.html/i }).click();
  await expect(secondPage.locator("#stored")).toHaveText("shared");
  await secondPage.goBack();

  await secondContext.setOffline(false);
  await secondPage.reload();
  await expect(secondPage.locator("#connectivity")).toHaveText("Online");
  await secondPage.getByRole("button", { name: "Manage" }).click();
  await secondPage.getByRole("button", { name: "Log out on this device" }).click();
  await expect(secondPage).toHaveURL("/");
  await secondContext.setOffline(true);
  await secondPage.goto("/app").catch(() => {});
  await expect(secondPage.getByRole("heading", { name: "Pocket tools" })).not.toBeVisible();
  await secondContext.close();
});
