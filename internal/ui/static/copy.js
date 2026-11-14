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
      // Out of the flow and one pixel big, so the page does not move or
      // scroll while the text is selected.
      holder.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;overflow:hidden;opacity:0;pointer-events:none;white-space:pre";
      holder.textContent = idButton.getAttribute("data-copy-text");
      document.body.appendChild(holder);
      var status = idButton.nextElementSibling;
      var idleTitle = idButton.getAttribute("data-idle-title");
      if (idleTitle === null) {
        idleTitle = idButton.getAttribute("aria-label");
        idButton.setAttribute("data-idle-title", idleTitle);
      }
      var show = function (state, message) {
        idButton.setAttribute("data-state", state);
        idButton.setAttribute("title", message);
        if (status && status.hasAttribute("data-copy-status")) {
          status.textContent = message;
        }
      };
      window.clearTimeout(idButton.copyTimer);
      copyText(holder.textContent, holder).then(
        function () { show("done", "Copied"); },
        function () { show("failed", "Copy failed"); }
      ).finally(function () {
        holder.remove();
        idButton.copyTimer = window.setTimeout(function () {
          idButton.removeAttribute("data-state");
          idButton.setAttribute("title", idleTitle);
          if (status && status.hasAttribute("data-copy-status")) {
            status.textContent = "";
          }
        }, 2000);
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
