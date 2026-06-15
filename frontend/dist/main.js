const statusText = document.getElementById("status-text");
const spinner = document.getElementById("spinner");
const queueList = document.getElementById("queue-list");
const queueCount = document.getElementById("queue-count");
const logs = document.getElementById("logs");

let queue = [];

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
    item.textContent = basename(path);
    item.title = path;
    queueList.appendChild(item);
  }
}

function setRunning(running) {
  spinner.classList.toggle("hidden", !running);
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
    statusText.textContent = status.message;
    setRunning(Boolean(status.running));
  });

  window.runtime.EventsOn("subtrans:log", (text) => {
    appendLog(String(text));
  });

  window.runtime.EventsOn("subtrans:queue:set", (paths) => {
    queue = Array.isArray(paths) ? paths.slice() : [];
    renderQueue();
  });

  window.runtime.EventsOn("subtrans:queue:remove", (path) => {
    queue = queue.filter((item) => item !== path);
    renderQueue();
  });

  window.runtime.OnFileDrop(() => {}, true);
}

renderQueue();
bindRuntime();
