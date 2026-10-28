var app = null;
// Handle pops
var handlePop = function (evt) {
    var path = evt.target.location.pathname;
    if (path === '/') {
        app.newDocument(true);
    } else {
        app.loadDocument(path.substring(1, path.length));
    }
};
// Set up the pop state to handle loads, skipping the first load
// to make chrome behave like others:
// http://code.google.com/p/chromium/issues/detail?id=63040
setTimeout(function () {
    window.onpopstate = function (evt) {
        try {
            handlePop(evt);
        } catch (err) { /* not loaded yet */
        }
    };
}, 1000);
// Construct app and load initial path
$(function () {
    app = new haste('hastebin', { twitter: false });
    app.loadInfo();
    handlePop({ target: window });
    if (window.location.pathname === '/') {
        app.restoreDraft();
    }
});
