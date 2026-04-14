const state = {
  clientId: localStorage.getItem("purple-lightswitch-client-id") || "",
  socket: null,
  selectedPreset: "",
  presetsExpanded: true,
  file: null,
  jobs: [],
  ignoredJobIds: new Set(),
};

const elements = {
  setupScreen: document.getElementById("setup-screen"),
  workingScreen: document.getElementById("working-screen"),
  doneScreen: document.getElementById("done-screen"),
  presetToggle: document.getElementById("preset-toggle"),
  presetSummary: document.getElementById("preset-summary"),
  presetSummaryName: document.getElementById("preset-summary-name"),
  presetSummaryTag: document.getElementById("preset-summary-tag"),
  presetList: document.getElementById("preset-list"),
  photoInput: document.getElementById("photo-input"),
  previewImage: document.getElementById("preview-image"),
  previewEmpty: document.getElementById("preview-empty"),
  transformButton: document.getElementById("transform-button"),
  setupNote: document.getElementById("setup-note"),
  workingTitle: document.getElementById("working-title"),
  workingDetail: document.getElementById("working-detail"),
  workingPreview: document.getElementById("working-preview"),
  workingPreviewEmpty: document.getElementById("working-preview-empty"),
  progressTrack: document.querySelector(".progress-track"),
  progressFill: document.getElementById("progress-fill"),
  progressLabel: document.getElementById("progress-label"),
  cancelButton: document.getElementById("cancel-button"),
  resultImage: document.getElementById("result-image"),
  resultEmpty: document.getElementById("result-empty"),
  downloadLink: document.getElementById("download-link"),
  reuseButton: document.getElementById("reuse-button"),
  restartButton: document.getElementById("restart-button"),
};

const presetButtons = Array.from(document.querySelectorAll(".preset"));

function setStage(stage) {
  elements.setupScreen.hidden = stage !== "setup";
  elements.workingScreen.hidden = stage !== "working";
  elements.doneScreen.hidden = stage !== "done";
}

function setSetupNote(message, mode = "") {
  elements.setupNote.textContent = message;
  elements.setupNote.className = `setup-note ${mode}`.trim();
}

function presetMeta(id) {
  const button = presetButtons.find((item) => item.dataset.preset === id);
  if (!button) {
    return null;
  }
  return {
    name: button.querySelector(".preset-name")?.textContent || "",
    tag: button.querySelector(".preset-tag")?.textContent || "",
  };
}

function renderPresetPicker() {
  const hasSelection = Boolean(state.selectedPreset);
  const meta = hasSelection ? presetMeta(state.selectedPreset) : null;

  elements.presetList.hidden = !state.presetsExpanded;
  elements.presetToggle.textContent = state.presetsExpanded ? "Hide choices" : "Change";
  elements.presetToggle.setAttribute("aria-expanded", String(state.presetsExpanded));

  if (!hasSelection || state.presetsExpanded || !meta) {
    elements.presetSummary.hidden = true;
    return;
  }

  elements.presetSummary.hidden = false;
  elements.presetSummaryName.textContent = meta.name;
  elements.presetSummaryTag.textContent = meta.tag;
}

function connectSocket() {
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const url = new URL(`${protocol}//${location.host}/ws`);
  if (state.clientId) {
    url.searchParams.set("client_id", state.clientId);
  }

  const socket = new WebSocket(url);
  state.socket = socket;

  socket.addEventListener("message", (event) => {
    const payload = JSON.parse(event.data);

    if (payload.type === "hello") {
      state.clientId = payload.clientId;
      localStorage.setItem("purple-lightswitch-client-id", state.clientId);
      if (!state.selectedPreset && payload.presets.length) {
        selectPreset(payload.presets[0].id, { collapse: false });
      }
      return;
    }

    if (payload.type === "jobs") {
      state.jobs = payload.jobs;
      renderJobState();
    }
  });

  socket.addEventListener("close", () => {
    state.socket = null;
    if (activeJob()) {
      setStage("working");
      elements.workingTitle.textContent = "Reconnecting";
      elements.workingDetail.textContent = "Waiting to reconnect to the server.";
      setIndeterminateProgress("Trying to reconnect...");
    } else {
      setSetupNote("Trying to reconnect to the server...", "busy");
    }
    window.setTimeout(connectSocket, 1200);
  });
}

function selectPreset(id, options = {}) {
  const collapse = options.collapse !== false;
  state.selectedPreset = id;
  if (collapse) {
    state.presetsExpanded = false;
  }
  presetButtons.forEach((button) => {
    button.classList.toggle("selected", button.dataset.preset === id);
  });
  renderPresetPicker();
}

function visibleJobs() {
  return state.jobs.filter((job) => !state.ignoredJobIds.has(job.id));
}

function activeJob() {
  return visibleJobs().find((job) => job.status === "running" || job.status === "queued") || null;
}

function currentJob() {
  return activeJob()
    || visibleJobs().find((job) => job.status === "completed")
    || visibleJobs()[0]
    || null;
}

function setProgress(current, total) {
  const safeTotal = total > 0 ? total : 1;
  const safeCurrent = Math.max(0, Math.min(current, safeTotal));
  const percent = Math.round((safeCurrent / safeTotal) * 100);
  elements.progressTrack.classList.remove("indeterminate");
  elements.progressFill.style.width = `${Math.max(8, percent)}%`;
  elements.progressLabel.textContent = `${percent}%`;
}

function phaseTitle(phase) {
  switch (phase) {
    case "vae":
      return "Reading your photo";
    case "buffer":
      return "Building the new look";
    case "generate":
      return "Painting the final image";
    default:
      return "Restyling in progress";
  }
}

function phaseDetail(phase, presetName) {
  switch (phase) {
    case "vae":
      return `Mapping the shot for ${presetName}.`;
    case "buffer":
      return `Transforming the scene into ${presetName}.`;
    case "generate":
      return `Finishing the ${presetName} version of your photo.`;
    default:
      return `Applying ${presetName}.`;
  }
}

function setWeightedProgress(job) {
  const percent = Math.max(0, Math.min(job.progressPercent || 0, 100));
  elements.progressTrack.classList.remove("indeterminate");
  elements.progressFill.style.width = `${Math.max(8, percent)}%`;
  elements.progressLabel.textContent = `${percent}%`;
}

function setIndeterminateProgress(label) {
  elements.progressTrack.classList.add("indeterminate");
  elements.progressFill.style.width = "";
  elements.progressLabel.textContent = label;
}

function showWorkingPreview() {
  if (!state.file) {
    elements.workingPreview.hidden = true;
    elements.workingPreview.removeAttribute("src");
    elements.workingPreviewEmpty.hidden = false;
    return;
  }
  elements.workingPreview.src = elements.previewImage.src;
  elements.workingPreview.hidden = false;
  elements.workingPreviewEmpty.hidden = true;
}

function renderWorking(job) {
  setStage("working");
  showWorkingPreview();

  if (job.status === "queued") {
    elements.workingTitle.textContent = "In line";
    if (job.queuePosition > 0) {
      elements.workingDetail.textContent = `${job.queuePosition} render${job.queuePosition === 1 ? "" : "s"} ahead before ${job.presetName}.`;
    } else {
      elements.workingDetail.textContent = `${job.presetName} is next.`;
    }
    setIndeterminateProgress("Waiting for a render slot");
    return;
  }

  elements.workingTitle.textContent = phaseTitle(job.progressPhase);
  elements.workingDetail.textContent = phaseDetail(job.progressPhase, job.presetName);
  if (job.progressPercent > 0) {
    setWeightedProgress(job);
  } else if (job.progressTotal > 0) {
    setProgress(job.progressCurrent, job.progressTotal);
  } else {
    setIndeterminateProgress("Renderer is working");
  }
}

function renderDone(job) {
  setStage("done");
  elements.resultImage.hidden = false;
  elements.resultImage.src = `${job.outputUrl}?t=${Date.now()}`;
  elements.resultEmpty.hidden = true;
  elements.downloadLink.hidden = false;
  elements.downloadLink.href = job.outputUrl;
}

function renderJobState() {
  const job = currentJob();
  elements.transformButton.disabled = !state.file || !state.selectedPreset;

  if (!job) {
    setStage("setup");
    setSetupNote(state.file ? "Photo ready. Pick a transformation." : "Pick a transformation and add a photo.");
    return;
  }

  if (job.status === "queued" || job.status === "running") {
    renderWorking(job);
    return;
  }

  if (job.status === "completed" && job.outputUrl) {
    renderDone(job);
    return;
  }

  setStage("setup");
  if (job.status === "canceled") {
    setSetupNote("Render canceled.");
    return;
  }
  setSetupNote(job.error || "That render failed. Try another preset.", "error");
}

async function submitTransform() {
  if (!state.file) {
    setSetupNote("Add a photo first.", "error");
    return;
  }
  if (!state.selectedPreset) {
    setSetupNote("Pick a transformation first.", "error");
    return;
  }

  const formData = new FormData();
  formData.set("client_id", state.clientId);
  formData.set("preset", state.selectedPreset);
  formData.set("photo", state.file);

  elements.transformButton.disabled = true;
  setStage("working");
  showWorkingPreview();
  elements.workingTitle.textContent = "Sending your photo";
  elements.workingDetail.textContent = "Uploading the shot to the renderer.";
  setIndeterminateProgress("Uploading");

  try {
    const response = await fetch("/api/transform", {
      method: "POST",
      body: formData,
    });
    if (!response.ok) {
      throw new Error(await response.text());
    }
    const payload = await response.json();
    if (payload.clientId) {
      state.clientId = payload.clientId;
      localStorage.setItem("purple-lightswitch-client-id", state.clientId);
    }
    if (payload.job && payload.job.id) {
      state.ignoredJobIds.delete(payload.job.id);
    }
    setIndeterminateProgress("Waiting for the renderer");
  } catch (error) {
    setStage("setup");
    elements.transformButton.disabled = false;
    setSetupNote(String(error.message || error), "error");
  }
}

async function cancelCurrentJob() {
  const job = activeJob();
  if (!job) {
    return;
  }

  elements.cancelButton.disabled = true;
  try {
    const response = await fetch(`/api/jobs/${job.id}/cancel`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ clientId: state.clientId }),
    });
    if (!response.ok) {
      throw new Error(await response.text());
    }
    state.ignoredJobIds.add(job.id);
    setStage("setup");
    setSetupNote("Render canceled.");
  } catch (error) {
    elements.workingDetail.textContent = String(error.message || error);
  } finally {
    elements.cancelButton.disabled = false;
  }
}

function clearPreview() {
  elements.previewImage.hidden = true;
  elements.previewImage.removeAttribute("src");
  elements.previewEmpty.hidden = false;
  elements.workingPreview.hidden = true;
  elements.workingPreview.removeAttribute("src");
  elements.workingPreviewEmpty.hidden = false;
}

function clearResult() {
  elements.resultImage.hidden = true;
  elements.resultImage.removeAttribute("src");
  elements.resultEmpty.hidden = false;
  elements.downloadLink.hidden = true;
  elements.downloadLink.removeAttribute("href");
}

function restart() {
  for (const job of visibleJobs()) {
    state.ignoredJobIds.add(job.id);
  }
  state.file = null;
  state.presetsExpanded = Boolean(state.selectedPreset) ? false : true;
  elements.photoInput.value = "";
  clearPreview();
  clearResult();
  setStage("setup");
  setSetupNote("Pick a transformation and add a photo.");
  renderJobState();
}

function reusePhoto() {
  for (const job of visibleJobs()) {
    state.ignoredJobIds.add(job.id);
  }
  clearResult();
  state.presetsExpanded = false;
  setStage("setup");
  if (state.file) {
    setSetupNote("Photo ready. Pick another transformation.");
  } else {
    setSetupNote("Add a photo first.");
  }
  renderJobState();
}

elements.presetList.addEventListener("click", (event) => {
  const button = event.target.closest(".preset");
  if (!button) {
    return;
  }
  selectPreset(button.dataset.preset);
  renderJobState();
});

elements.presetToggle.addEventListener("click", () => {
  state.presetsExpanded = !state.presetsExpanded;
  renderPresetPicker();
});

elements.photoInput.addEventListener("change", () => {
  const file = elements.photoInput.files && elements.photoInput.files[0];
  if (!file) {
    return;
  }

  state.file = file;
  elements.previewImage.src = URL.createObjectURL(file);
  elements.previewImage.hidden = false;
  elements.previewEmpty.hidden = true;
  setSetupNote("Photo loaded. Pick a transformation.");
  renderJobState();
});

elements.transformButton.addEventListener("click", submitTransform);
elements.cancelButton.addEventListener("click", cancelCurrentJob);
elements.reuseButton.addEventListener("click", reusePhoto);
elements.restartButton.addEventListener("click", restart);

setStage("setup");
setSetupNote("Pick a transformation and add a photo.");
renderPresetPicker();
connectSocket();
