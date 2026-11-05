"use strict";
const $ = (selector) => document.querySelector(selector);
const key = new URLSearchParams(location.hash.slice(1)).get("key") || "";
let phase = "ready",
  polling = false,
  pathLoaded = false;
async function request(path, body) {
  const response = await fetch(path, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json", "X-UULink-CSRF": key },
    body: body ? JSON.stringify(body) : undefined,
    cache: "no-store",
  });
  const data = await response.json();
  if (!response.ok) throw Error(data.error || "操作未完成，请重试");
  return data;
}
function showError(selector, message) {
  const box = $(selector);
  box.textContent = message;
  box.hidden = !message;
}
function applyStatus(status) {
  const changed = status.phase !== phase;
  if (changed) {
    phase = status.phase;
    for (const name of ["ready", "installing", "done", "error"])
      $("#" + name).hidden = name !== phase;
  }
  const step = phase === "done" ? 2 : phase === "ready" ? 0 : 1;
  document.querySelectorAll(".steps li").forEach((item, index) => {
    item.classList.toggle("active", index === step);
    item.classList.toggle("complete", index < step);
    if (index === step) item.setAttribute("aria-current", "step");
    else item.removeAttribute("aria-current");
  });
  if (phase === "ready" && !pathLoaded) {
    $("#install-path").value = status.path;
    pathLoaded = true;
  }
  $("#existing-note").textContent = status.updating
    ? "已安装 UU Link，本次将更新程序。"
    : "准备就绪后即可开始安装。";
  $("#progress-message").textContent = status.message || "正在准备安装文件";
  $("#error-message").textContent = status.error || "";
  $("#error-path").textContent = status.path;
  document
    .querySelectorAll("[data-window=close]")
    .forEach((button) => (button.disabled = phase === "installing"));
  if (changed && phase === "error") $("#retry").focus();
}
async function refresh() {
  if (polling) return;
  polling = true;
  try {
    applyStatus(await request("/api/status"));
  } catch (error) {
    if (phase === "ready") showError("#ready-error", error.message);
  } finally {
    polling = false;
  }
}
async function install() {
  const button = phase === "error" ? $("#retry") : $("#install");
  button.disabled = true;
  showError("#ready-error", "");
  try {
    await request("/api/install", { path: $("#install-path").value.trim() });
    await refresh();
  } catch (error) {
    showError("#ready-error", error.message);
  } finally {
    button.disabled = false;
  }
}
$("#install").addEventListener("click", install);
$("#retry").addEventListener("click", install);
$("#change-path").addEventListener("click", () => {
  const input = $("#install-path");
  input.readOnly = !input.readOnly;
  $("#change-path").textContent = input.readOnly ? "更改" : "收起";
  if (!input.readOnly) {
    input.focus();
    input.select();
  }
});
document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-window]");
  if (!button) return;
  try {
    await request("/api/platform/preferences", {
      key: "window-" + button.dataset.window,
      value: true,
    });
  } catch (error) {
    showError("#ready-error", error.message);
  }
});
$("#finish").addEventListener("click", async () => {
  const button = $("#finish");
  button.disabled = true;
  try {
    if ($("#launch").checked) await request("/api/launch", {});
    await request("/api/platform/preferences", {
      key: "window-exit",
      value: true,
    });
  } catch (error) {
    showError("#done-error", error.message);
    button.disabled = false;
  }
});
document.querySelector(".top").addEventListener("mousedown", (event) => {
  if (
    window.uuNativeDrag ||
    event.button !== 0 ||
    event.target.closest("button")
  )
    return;
  event.preventDefault();
  request("/api/platform/preferences", {
    key: "window-drag",
    value: true,
  }).catch(() => {});
});
window.uulinkWindowState = (maximized) =>
  document.body.classList.toggle("win-maximized", maximized);
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () =>
  request("/api/platform/preferences", {
    key: "appearance-dark",
    value: matchMedia("(prefers-color-scheme: dark)").matches,
  }).catch(() => {}),
);
(async () => {
  try {
    await request("/api/session", { key });
    history.replaceState(null, "", location.pathname);
    await refresh();
    setInterval(refresh, 600);
  } catch (error) {
    showError("#ready-error", error.message);
    $("#install").disabled = true;
  }
})();
