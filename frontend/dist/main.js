const statusText = document.getElementById("status-text");
const spinner = document.getElementById("spinner");
const queueList = document.getElementById("queue-list");
const queueCount = document.getElementById("queue-count");
const toggleBtn = document.getElementById("toggle-btn");
const logs = document.getElementById("logs");

let queue = [];
let activePath = null;
let running = false;
let paused = false;

function basename(path) {
  return String(path).split(/[\\/]/).filter(Boolean).pop() || path;
}

function renderQueue() {
  queueCount.textContent = String(queue.length);
  queueList.replaceChildren();

  if (queue.length === 0) {
    const empty = document.createElement("li");
    empty.className = "empty";
    empty.textContent = "No queued videos";
    queueList.appendChild(empty);
    return;
  }

  for (const path of queue) {
    const item = document.createElement("li");
    item.title = path;

    if (path === activePath) {
      item.classList.add("processing");
      const dot = document.createElement("span");
      dot.className = "dot";
      item.appendChild(dot);
    }

    const label = document.createElement("span");
    label.className = "name";
    label.textContent = basename(path);
    item.appendChild(label);

    const remove = document.createElement("button");
    remove.className = "remove";
    remove.type = "button";
    remove.title = "Remove from queue";
    remove.setAttribute("aria-label", "Remove from queue");
    remove.textContent = "×";
    remove.addEventListener("click", () => {
      window.runtime.EventsEmit("subtrans:control:remove", path);
    });
    item.appendChild(remove);

    queueList.appendChild(item);
  }
}

function updateControls() {
  // The toggle button only makes sense while a run is active (running or paused).
  toggleBtn.classList.toggle("hidden", !(running || paused));
  toggleBtn.textContent = paused ? "Resume" : "Stop";
  spinner.classList.toggle("hidden", !running || paused);
}

function appendLog(text) {
  logs.textContent += text;
  logs.scrollTop = logs.scrollHeight;
}

function bindRuntime() {
  if (!window.runtime) {
    setTimeout(bindRuntime, 30);
    return;
  }

  window.runtime.EventsOn("subtrans:status", (status) => {
    // Status events only fire at run start and completion (pause uses a
    // separate event), so either way the queue is no longer paused.
    running = Boolean(status.running);
    paused = false;
    statusText.textContent = status.message;
    updateControls();
  });

  window.runtime.EventsOn("subtrans:paused", (value) => {
    paused = Boolean(value);
    statusText.textContent = paused ? "Paused" : "Running...";
    updateControls();
  });

  window.runtime.EventsOn("subtrans:log", (text) => {
    appendLog(String(text));
  });

  window.runtime.EventsOn("subtrans:queue:set", (paths) => {
    queue = Array.isArray(paths) ? paths.slice() : [];
    activePath = null;
    renderQueue();
  });

  window.runtime.EventsOn("subtrans:queue:active", (path) => {
    activePath = path;
    renderQueue();
  });

  window.runtime.EventsOn("subtrans:queue:remove", (path) => {
    queue = queue.filter((item) => item !== path);
    if (activePath === path) {
      activePath = null;
    }
    renderQueue();
  });

  toggleBtn.addEventListener("click", () => {
    window.runtime.EventsEmit("subtrans:control:toggle");
  });

  window.runtime.OnFileDrop(() => {}, true);

  // Signal the backend that listeners are registered so it can start the
  // initial run without racing the emitted status/queue events.
  window.runtime.EventsEmit("subtrans:ready");
}

renderQueue();
updateControls();
bindRuntime();
