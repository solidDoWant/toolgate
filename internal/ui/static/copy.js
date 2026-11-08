// Copy buttons: the command blocks on the Quick setup page, and the icon
// button next to a shortened ID.
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
    var idButton = event.target.closest("button[data-copy-text]");
    if (idButton) {
      var holder = document.createElement("span");
      holder.textContent = idButton.getAttribute("data-copy-text");
      document.body.appendChild(holder);
      copyText(holder.textContent, holder).then(
        function () { idButton.setAttribute("title", "Copied"); },
        function () { idButton.setAttribute("title", "Copy failed"); }
      ).finally(function () {
        holder.remove();
        window.setTimeout(function () { idButton.setAttribute("title", "Copy the full ID"); }, 2000);
      });
      return;
    }
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
