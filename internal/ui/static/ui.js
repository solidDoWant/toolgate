// Behaviour of the web UI's shared components: dialogs, token inputs and
// filter bars.
// Every page works without this file: a dialog's link goes to the form's
// own page, and a token input is a textarea with one item per line.
(function () {
  "use strict";

  var focusable = 'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

  function visibleFocusable(container) {
    return Array.prototype.filter.call(container.querySelectorAll(focusable), function (element) {
      return element.offsetParent !== null || element === document.activeElement;
    });
  }

  // Dialogs. A link with data-dialog opens the dialog of that id as a modal;
  // closing it returns the focus to the link.
  document.addEventListener("click", function (event) {
    var opener = event.target.closest("[data-dialog]");
    if (opener) {
      var dialog = document.getElementById(opener.getAttribute("data-dialog"));
      if (!dialog || typeof dialog.showModal !== "function") {
        return;
      }
      event.preventDefault();
      dialog.returnFocus = opener;
      dialog.showModal();
      var form = dialog.querySelector("form") || dialog;
      var first = visibleFocusable(form)[0];
      if (first) {
        first.focus();
      }
      return;
    }
    var closer = event.target.closest("[data-close-dialog]");
    if (closer && closer.closest("dialog[open]")) {
      event.preventDefault();
      closer.closest("dialog").close();
    }
  });

  document.addEventListener("close", function (event) {
    var dialog = event.target;
    if (dialog.tagName === "DIALOG" && dialog.returnFocus) {
      dialog.returnFocus.focus();
      dialog.returnFocus = null;
    }
  }, true);

  // Keep Tab inside an open modal: from the last control back to the first.
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Tab") {
      return;
    }
    var dialog = document.querySelector("dialog[open]");
    if (!dialog) {
      return;
    }
    var elements = visibleFocusable(dialog);
    if (elements.length === 0) {
      return;
    }
    var first = elements[0];
    var last = elements[elements.length - 1];
    if (event.shiftKey && (document.activeElement === first || !dialog.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (document.activeElement === last || !dialog.contains(document.activeElement))) {
      event.preventDefault();
      first.focus();
    }
  });

  // Token inputs. The textarea stays in the form, hidden, and holds the
  // items one per line; the chips and the text box edit it.
  function tokenInput(textarea) {
    var items = textarea.value.split("\n").map(function (item) { return item.trim(); }).filter(Boolean);
    var box = document.createElement("div");
    box.className = "token-input";
    var input = document.createElement("input");
    input.type = "text";
    input.id = textarea.id;
    input.placeholder = textarea.getAttribute("data-placeholder") || "";
    input.setAttribute("aria-describedby", textarea.getAttribute("aria-describedby") || "");
    input.setAttribute("autocomplete", "off");
    textarea.id = textarea.id + "-items";
    textarea.hidden = true;
    textarea.tabIndex = -1;

    function sync() {
      textarea.value = items.join("\n");
      Array.prototype.forEach.call(box.querySelectorAll(".token"), function (chip) { chip.remove(); });
      items.forEach(function (item, index) {
        var chip = document.createElement("span");
        chip.className = "token";
        chip.textContent = item;
        var remove = document.createElement("button");
        remove.type = "button";
        remove.textContent = "×";
        remove.setAttribute("aria-label", "Remove " + item);
        remove.addEventListener("click", function () {
          items.splice(index, 1);
          sync();
          input.focus();
        });
        chip.appendChild(remove);
        box.insertBefore(chip, input);
      });
    }

    function add() {
      var item = input.value.trim();
      if (item !== "" && items.indexOf(item) < 0) {
        items.push(item);
      }
      input.value = "";
      sync();
    }

    input.addEventListener("keydown", function (event) {
      if (event.key === "Enter") {
        event.preventDefault();
        add();
      } else if (event.key === "Backspace" && input.value === "" && items.length > 0) {
        items.pop();
        sync();
      }
    });
    // Enter's keypress submits the form unless it is stopped too.
    input.addEventListener("keypress", function (event) {
      if (event.key === "Enter") {
        event.preventDefault();
      }
    });
    input.addEventListener("blur", add);
    if (textarea.form) {
      textarea.form.addEventListener("submit", add);
    }
    box.addEventListener("click", function (event) {
      if (event.target === box) {
        input.focus();
      }
    });

    box.appendChild(input);
    textarea.parentNode.insertBefore(box, textarea);
    sync();
  }

  // Filter bars. A change submits the form, so filters apply as they are
  // set. A control inside a popover waits until the popover closes, so
  // several can be set at once. Without this script the Filter button
  // submits the form.
  var pending = new WeakSet();

  document.addEventListener("change", function (event) {
    var form = event.target.closest("form[data-autosubmit]");
    if (!form || event.target.hasAttribute("data-no-submit")) {
      return;
    }
    if (event.target.matches("[data-range]")) {
      var custom = form.querySelector("[data-custom-range]");
      if (custom) {
        custom.hidden = event.target.value !== "custom";
      }
      if (event.target.value === "custom") {
        return;
      }
    }
    if (event.target.closest("details[data-popover]")) {
      pending.add(form);
      return;
    }
    form.requestSubmit();
  });

  document.addEventListener("toggle", function (event) {
    var popover = event.target;
    if (!popover.matches || !popover.matches("details[data-popover]")) {
      return;
    }
    var form = popover.closest("form[data-autosubmit]");
    if (popover.open) {
      var search = popover.querySelector("[data-type-search]");
      if (search) {
        search.hidden = false;
        search.focus();
      }
    } else if (form && pending.has(form)) {
      pending.delete(form);
      form.requestSubmit();
    }
  }, true);

  // A popover closes on a click outside it and on Escape.
  document.addEventListener("click", function (event) {
    Array.prototype.forEach.call(document.querySelectorAll("details[data-popover][open]"), function (popover) {
      if (!popover.contains(event.target)) {
        popover.open = false;
      }
    });
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      Array.prototype.forEach.call(document.querySelectorAll("details[data-popover][open]"), function (popover) {
        popover.open = false;
      });
    }
  });

  // The event type search hides the choices that don't match, and a
  // category left with none.
  document.addEventListener("input", function (event) {
    if (!event.target.matches("[data-type-search]")) {
      return;
    }
    var query = event.target.value.trim().toLowerCase();
    var list = event.target.parentNode.querySelector("[data-type-list]");
    Array.prototype.forEach.call(list.querySelectorAll("[data-type-group]"), function (group) {
      var shown = 0;
      Array.prototype.forEach.call(group.querySelectorAll("[data-type-item]"), function (item) {
        var match = query === "" || item.textContent.toLowerCase().indexOf(query) >= 0 || item.title.toLowerCase().indexOf(query) >= 0;
        item.hidden = !match;
        if (match) {
          shown++;
        }
      });
      group.hidden = shown === 0;
    });
  });

  function start() {
    Array.prototype.forEach.call(document.querySelectorAll("form[data-autosubmit] [data-filter-submit]"), function (button) { button.hidden = true; });
    Array.prototype.forEach.call(document.querySelectorAll("textarea[data-token-input]"), tokenInput);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
