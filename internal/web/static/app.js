// An action that takes something away asks twice in place: the first click
// turns its button into the sentence that states the effect, the second
// does it, and a click anywhere else leaves things as they are.
(function () {
  "use strict";
  var armed = null;

  function disarm() {
    if (!armed) return;
    armed.button.textContent = armed.label;
    armed.button.classList.remove("armed");
    armed.sure.remove();
    armed = null;
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("button[data-sentence]") : null;
    if (armed && armed.button === button) return;
    disarm();
    if (!button || !button.form) return;
    event.preventDefault();
    var sure = document.createElement("input");
    sure.type = "hidden";
    sure.name = "sure";
    sure.value = "yes";
    button.form.appendChild(sure);
    armed = { button: button, label: button.textContent, sure: sure };
    button.textContent = button.dataset.sentence;
    button.classList.add("armed");
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") disarm();
  });
})();

// A value a person is meant to copy has a Copy button beside it, rendered
// hidden so that a browser that runs no script shows the text alone, still
// selectable. Here the buttons are shown; a click copies the text marked
// data-copy in the same .copyable, or, where the clipboard is refused,
// selects it. The button says what happened for two seconds, and a status
// line says it to a screen reader.
(function () {
  "use strict";
  var buttons = document.querySelectorAll("button.copy-button");
  if (!buttons.length) return;
  var status = document.createElement("span");
  status.className = "visually-hidden";
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");
  document.body.appendChild(status);
  for (var i = 0; i < buttons.length; i++) buttons[i].hidden = false;

  var timers = new WeakMap();

  function say(button, word) {
    clearTimeout(timers.get(button));
    button.textContent = word;
    status.textContent = "";
    window.setTimeout(function () { status.textContent = word; }, 50);
    timers.set(button, window.setTimeout(function () {
      button.textContent = "Copy";
      status.textContent = "";
    }, 2000));
  }

  function select(target) {
    var range = document.createRange();
    range.selectNodeContents(target);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    try {
      return document.execCommand("copy");
    } catch (e) {
      return false;
    }
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("button.copy-button") : null;
    var group = button && button.closest(".copyable");
    var target = group && group.querySelector("[data-copy]");
    if (!target) return;
    var text = target.textContent;
    function fallback() { say(button, select(target) ? "Copied" : "Selected"); }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { say(button, "Copied"); }, fallback);
    } else {
      fallback();
    }
  });
})();
