/* The guide exists in English (help.html) and Russian (help.ru.html).
   The first visit follows the browser language; a language chosen from the
   switcher is remembered, so the next visit keeps it. */
(function () {
  var KEY = "x-node-help-lang";
  var params = new URLSearchParams(location.search);
  var requested = params.get("lang");
  if (requested === "en" || requested === "ru") {
    try { localStorage.setItem(KEY, requested); } catch (_) { /* private mode */ }
  }
  var saved = null;
  try { saved = localStorage.getItem(KEY); } catch (_) { /* private mode */ }
  var lang = saved || ((navigator.language || "en").toLowerCase().indexOf("ru") === 0 ? "ru" : "en");
  var isRu = /help\.ru(\.html)?$/.test(location.pathname);
  if (lang === "ru" && !isRu) {
    location.replace("help.ru.html" + (requested ? "?lang=ru" : ""));
  } else if (lang === "en" && isRu) {
    location.replace("help.html" + (requested ? "?lang=en" : ""));
  }
})();
