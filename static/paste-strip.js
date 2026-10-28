// The strip under the button bar of a paste. Its owner gets
// "Edit (m:ss left) | Link or Private | Dashboard"; everyone sees
// "edited at HH:MM" once the owner changed the text under the same link.
// Values from the server are set with .text() and attributes only.
var paste_strip = (function () {
    'use strict';

    var timer = null;
    var deadline = 0;
    var key = null;
    var visibility = 'link';
    var busy = false;

    function $s(sel) {
        return jQuery('#paste-strip').find(sel);
    }

    function pad(n) {
        return (n < 10 ? '0' : '') + n;
    }

    function stop() {
        if (timer) {
            clearInterval(timer);
            timer = null;
        }
    }

    function hide() {
        stop();
        key = null;
        jQuery('#paste-strip').prop('hidden', true);
    }

    // show draws the strip for the answer of /documents/<key>, or the same
    // fields from the dashboard API
    function show(meta, k) {
        hide();
        if (!meta) {
            return;
        }
        var owner = meta.owner === true;
        var edited = meta.edited_at ? new Date(meta.edited_at) : null;
        if (edited && isNaN(edited.getTime())) {
            edited = null;
        }
        if (!owner && !edited) {
            return;
        }
        key = k;
        $s('.ps-owner').prop('hidden', !owner);
        $s('.ps-edited').prop('hidden', !edited)
            .text(edited ? 'edited at ' + pad(edited.getHours()) + ':' + pad(edited.getMinutes()) : '')
            .attr('title', edited ? edited.toLocaleString() : '');
        if (owner) {
            $s('.ps-edit').attr('href', '/dashboard/p/' + encodeURIComponent(k));
            visibility = meta.visibility === 'private' ? 'private' : 'link';
            drawVisibility();
            var until = meta.editable_until ? new Date(meta.editable_until).getTime() : NaN;
            var now = meta.server_now ? new Date(meta.server_now).getTime() : NaN;
            // The time left by the server's clock, counted down on this one
            deadline = performance.now() + (until - now);
            if (!isNaN(until) && !isNaN(now) && until > now) {
                tick();
                timer = setInterval(tick, 1000);
            } else {
                drawEdit(0);
            }
        }
        jQuery('#paste-strip').prop('hidden', false);
    }

    function drawEdit(left) {
        var open = left > 0;
        var s = Math.ceil(left / 1000);
        $s('.ps-edit-text').text(open ? 'Edit (' + Math.floor(s / 60) + ':' + pad(s % 60) + ' left)' : 'Edit');
        $s('.ps-edit').toggleClass('open', open).attr('title', open
            ? 'Change the text under this same link until the time runs out or someone else opens it'
            : 'Changes go to a new version with its own link');
    }

    function tick() {
        var left = deadline - performance.now();
        if (left <= 0) {
            stop();
        }
        drawEdit(left);
    }

    function drawVisibility() {
        var priv = visibility === 'private';
        $s('.ps-vis').toggleClass('private', priv)
            .attr('aria-label', priv ? 'Private: only you can open it. Make it open to anyone with the link'
                : 'Link: anyone with the link can open it. Make it private');
        $s('.ps-vis-text').text(priv ? 'Private' : 'Link');
    }

    // The CSRF token of the session: from the page, else asked for
    function csrf(done) {
        if (haste_document.csrf) {
            done(haste_document.csrf);
            return;
        }
        jQuery.ajax('/api/v1/session', {dataType: 'json'}).done(function (s) {
            done(s && s.csrf ? s.csrf : null);
        }).fail(function () {
            done(null);
        });
    }

    function toggleVisibility() {
        if (!key || busy) {
            return;
        }
        var next = visibility === 'private' ? 'link' : 'private';
        var k = key;
        busy = true;
        csrf(function (token) {
            jQuery.ajax('/api/v1/pastes/' + encodeURIComponent(k), {
                type: 'PATCH', dataType: 'json', contentType: 'application/json',
                data: JSON.stringify({visibility: next}), headers: {'X-CSRF-Token': token || ''}
            }).done(function (item) {
                if (k !== key) {
                    return;
                }
                visibility = item.visibility;
                drawVisibility();
                if (app) {
                    app.showMessage(visibility === 'private' ? 'Private: only you can open it now.'
                        : 'Anyone with the link can open it now.');
                }
            }).fail(function (xhr) {
                var body = xhr.responseJSON;
                if (app) {
                    app.showMessage(body && body.message ? body.message : 'Changing who can open it failed.', 'error');
                }
            }).always(function () {
                busy = false;
            });
        });
    }

    // load asks the dashboard API about a paste just saved: the strip shows
    // only when it is the session's own
    function load(k) {
        jQuery.ajax('/api/v1/pastes/' + encodeURIComponent(k), {dataType: 'json'}).done(function (p) {
            p.owner = true;
            show(p, k);
        });
    }

    jQuery(function () {
        jQuery('#paste-strip').on('click', '.ps-vis', toggleVisibility);
    });

    return {show: show, hide: hide, load: load};
})();
