// Copy buttons for the command blocks on the Quick setup page.
(function () {
  "use strict";

  function copyText(text, pre) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    var range = document.createRange();
    range.selectNodeContents(pre);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    return document.execCommand("copy") ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest("button.copy");
    if (!button) {
      return;
    }
    var pre = button.parentElement.querySelector("pre");
    copyText(pre.textContent, pre).then(
      function () { button.textContent = "Copied"; },
      function () { button.textContent = "Press Ctrl+C"; }
    );
    window.setTimeout(function () { button.textContent = "Copy"; }, 2000);
  });
})();
