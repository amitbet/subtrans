const statusText = document.getElementById("status-text");
const spinner = document.getElementById("spinner");
const queueList = document.getElementById("queue-list");
const queueCount = document.getElementById("queue-count");
const logs = document.getElementById("logs");

let queue = [];
let activePath = null;

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
      item.className = "processing";
      const dot = document.createElement("span");
      dot.className = "dot";
      const label = document.createElement("span");
      label.className = "name";
      label.textContent = basename(path);
      item.append(dot, label);
    } else {
      item.textContent = basename(path);
    }

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

  window.runtime.OnFileDrop(() => {}, true);

  // Signal the backend that listeners are registered so it can start the
  // initial run without racing the emitted status/queue events.
  window.runtime.EventsEmit("subtrans:ready");
}

renderQueue();
bindRuntime();
