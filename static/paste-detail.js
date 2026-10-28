// The page of one paste in the dashboard, /dashboard/p/<key>: its details,
// the editor with the edit window countdown, versions and edit history.
// dashboard.js opens it with its helpers. Every value from the server is set
// with .text(), .val() or attributes only, never as HTML.
var paste_detail = (function () {
    'use strict';

    // Larger pastes are not loaded into the editor: a textarea that big freezes the page
    var EDIT_LIMIT = 1 << 20;
    // Languages offered in the list; any other can be typed
    var LANGUAGES = ['ansi', 'bash', 'c', 'cpp', 'css', 'diff', 'dts', 'go', 'html', 'ini', 'java', 'javascript', 'json',
        'makefile', 'markdown', 'perl', 'php', 'python', 'ruby', 'rust', 'sql', 'text', 'toml', 'typescript', 'xml', 'yaml'];

    var ui = null;
    var key = null;
    var paste = null;
    var original = null; // the content as loaded, null while not loaded
    var seq = 0;
    var timer = null;
    var deadline = 0; // performance.now() when the window ends
    var closed = null; // null while the window is open, else "seen" or "expired"
    var saving = false;
    var $root = null;

    function $(sel) {
        return $root.find(sel);
    }

    function pad(n) {
        return (n < 10 ? '0' : '') + n;
    }

    // HH:MM in the browser's time zone
    function clock(iso) {
        var t = new Date(iso);
        return isNaN(t.getTime()) ? '' : pad(t.getHours()) + ':' + pad(t.getMinutes());
    }

    function mmss(ms) {
        var s = Math.max(Math.ceil(ms / 1000), 0);
        return Math.floor(s / 60) + ':' + pad(s % 60);
    }

    function enc(k) {
        return encodeURIComponent(k);
    }

    function dirty() {
        return original !== null && jQuery('#d-text').val() !== original;
    }

    // ---- load

    function open(k, helpers) {
        ui = helpers;
        $root = jQuery('#tab-detail');
        key = k;
        paste = null;
        original = null;
        closed = null;
        stopTimer();
        var mine = ++seq;
        $root.addClass('busy').attr('aria-busy', 'true');
        $('.back').attr('href', ui.listPath()).attr('data-nav', '1');
        $('#detail-title').text(k);
        $('.d-key').text(k);
        jQuery('#d-text').val('').prop('disabled', true);
        $('.d-dirty, .meta-status').text('');
        $('.d-delete-prev input').prop('checked', false);
        ui.api('GET', '/api/v1/pastes/' + enc(k)).done(function (res) {
            if (mine !== seq) {
                return;
            }
            render(res);
            loadContent(mine);
        }).fail(function (xhr) {
            if (mine !== seq || xhr.status === 401) {
                return;
            }
            $root.removeClass('busy').removeAttr('aria-busy');
            ui.notice(xhr.status === 404 ? 'This paste does not exist, or it is not yours.' : ui.errorText(xhr, 'Could not load this paste.'));
            $('.detail-grid, .detail-actions').prop('hidden', true);
        });
    }

    function loadContent(mine) {
        var $text = jQuery('#d-text');
        if (paste.size > EDIT_LIMIT) {
            $('.d-large').text('This paste is ' + ui.bytes(paste.size) + ', too large to edit here. Download it, change it and upload it again.').prop('hidden', false);
            $text.prop('hidden', true);
            $root.removeClass('busy').removeAttr('aria-busy');
            updateSave();
            return;
        }
        $('.d-large').prop('hidden', true);
        $text.prop('hidden', false);
        // The owner's own read: it never ends the edit window
        jQuery.ajax('/raw/' + enc(key), {dataType: 'text', cache: false}).done(function (data) {
            if (mine !== seq) {
                return;
            }
            original = data;
            $text.val(data).prop('disabled', false);
            updateSave();
        }).fail(function (xhr) {
            if (mine === seq) {
                ui.notice(ui.errorText(xhr, 'Could not load the content of this paste.'));
            }
        }).always(function () {
            if (mine === seq) {
                $root.removeClass('busy').removeAttr('aria-busy');
            }
        });
    }

    function close() {
        seq++;
        stopTimer();
        key = null;
    }

    // ---- render

    function render(res) {
        paste = res;
        $('.detail-grid, .detail-actions').prop('hidden', false);
        var name = res.title || res.key;
        document.title = 'hastebin - ' + name;
        $('#detail-title').text(name).toggleClass('untitled', !res.title);
        $('.d-key').text(res.key);
        $('.d-vis').text(res.visibility === 'private' ? 'private' : 'link')
            .toggleClass('private', res.visibility === 'private');
        $('.d-ver').text('v' + res.version).prop('hidden', res.version <= 1 && !res.next_key);
        $('.d-edited').text(res.edit_count === 1 ? 'edited once' : 'edited ' + res.edit_count + ' times')
            .prop('hidden', !res.edit_count);
        var view = '/' + enc(res.key);
        $('.d-open').attr('href', view);
        $('.d-download').attr('href', '/raw' + view + '?download=1');

        renderMeta(res);
        renderStats(res);
        renderVersions(res);
        renderHistory(res);
        startWindow(res);
    }

    function renderMeta(p) {
        var $f = $('.meta-form');
        $f.find('[name=title]').val(p.title);
        $f.find('[name=language]').val(p.language);
        var $list = jQuery('#d-languages').empty();
        LANGUAGES.forEach(function (l) {
            $list.append(jQuery('<option></option>').val(l));
        });
        $f.find('[name=visibility][value=' + (p.visibility === 'private' ? 'private' : 'link') + ']').prop('checked', true);
        $f.find('[name=expiry]').val(p.expires_at ? 'date' : 'never');
        $f.find('[name=expires_at]').val(p.expires_at ? localInput(p.expires_at) : '').prop('hidden', !p.expires_at);
        $f.find('[name=tags]').val((p.tags || []).join(', '));
    }

    // The value of a datetime-local input for an ISO time
    function localInput(iso) {
        var t = new Date(iso);
        if (isNaN(t.getTime())) {
            return '';
        }
        return t.getFullYear() + '-' + pad(t.getMonth() + 1) + '-' + pad(t.getDate()) + 'T' + pad(t.getHours()) + ':' + pad(t.getMinutes());
    }

    function stat($dl, label, value, title) {
        var $dd = jQuery('<dd></dd>').text(value);
        if (title) {
            $dd.attr('title', title);
        }
        $dl.append(jQuery('<dt></dt>').text(label), $dd);
    }

    function renderStats(p) {
        var $dl = $('.d-stats').empty();
        stat($dl, 'Size', ui.bytes(p.size));
        stat($dl, 'Views', ui.count(p.views));
        stat($dl, 'Last opened', p.last_opened_at ? ui.short(p.last_opened_at) : 'never', ui.full(p.last_opened_at));
        stat($dl, 'Created', ui.short(p.created_at), ui.full(p.created_at));
        stat($dl, 'Edited', p.edited_at ? 'at ' + clock(p.edited_at) + ', ' + ui.plural(p.edit_count, 'time', 'times') : 'never',
            ui.full(p.edited_at));
        stat($dl, 'Expires', p.expires_at ? ui.full(p.expires_at) : 'never');
    }

    function renderVersions(p) {
        var $list = $('.chain').empty();
        var byNumber = {};
        var top = 0;
        (p.versions || []).forEach(function (v) {
            byNumber[v.version] = v;
            top = Math.max(top, v.version);
        });
        // At most 100 come back; numbers missing in between were deleted
        for (var n = 1; n <= top; n++) {
            var v = byNumber[n];
            var $li = jQuery('<li></li>');
            var $num = jQuery('<span class="v-num"></span>').text('v' + n);
            if (!v) {
                $li.addClass('gone').append(jQuery('<span></span>').append($num, jQuery('<span class="v-when"></span>').text('deleted')));
            } else if (v.key === p.key) {
                $li.addClass('current').attr('aria-current', 'true').append(jQuery('<span></span>')
                    .append($num, jQuery('<span class="v-when"></span>').text(ui.short(v.created_at) + ', this one')));
            } else {
                var $a = jQuery('<a data-nav="1"></a>').attr('href', '/dashboard/p/' + enc(v.key))
                    .append($num, jQuery('<span class="v-when"></span>').text(ui.short(v.created_at)));
                $li.append($a);
            }
            $list.append($li);
        }
    }

    function renderHistory(p) {
        var $list = $('.history').empty();
        var revs = (p.revisions || []).slice().reverse();
        if (revs.length) {
            var $now = jQuery('<li class="current"></li>');
            $now.append(jQuery('<span class="h-when"></span>').text(clock(p.edited_at || p.created_at)),
                jQuery('<span class="h-size"></span>').text(ui.bytes(p.size)),
                jQuery('<span class="h-what"></span>').text('current'));
            $list.append($now);
        }
        revs.forEach(function (r) {
            var $li = jQuery('<li></li>');
            var $view = jQuery('<a target="_blank" rel="noopener"></a>')
                .attr('href', '/api/v1/pastes/' + enc(p.key) + '/revisions/' + r.rev).text('View')
                .attr('aria-label', 'View the text saved before ' + clock(r.created_at));
            $li.append(jQuery('<span class="h-when"></span>').text(clock(r.created_at)).attr('title', ui.full(r.created_at)),
                jQuery('<span class="h-size"></span>').text(ui.bytes(r.size)), $view);
            $list.append($li);
        });
        $('.history-empty').prop('hidden', revs.length > 0);
    }

    // ---- edit window

    function stopTimer() {
        if (timer) {
            clearInterval(timer);
            timer = null;
        }
    }

    function startWindow(p) {
        stopTimer();
        closed = p.edit_closed || null;
        if (!closed && p.max_revisions > 0 && (p.revisions || []).length >= p.max_revisions) {
            closed = 'revisions';
        }
        if (!closed && p.editable_until && p.server_now) {
            // Server time only: the length left, counted on this browser's monotonic clock
            var left = new Date(p.editable_until).getTime() - new Date(p.server_now).getTime();
            deadline = performance.now() + left;
            if (left <= 0) {
                closed = 'expired';
            }
        } else if (!closed) {
            closed = 'expired';
        }
        tick();
        if (!closed) {
            timer = setInterval(tick, 1000);
        }
    }

    function tick() {
        var $state = $('.window-state');
        var left = deadline - performance.now();
        if (!closed && left <= 0) {
            closed = 'expired';
            stopTimer();
        }
        $root.toggleClass('window-open', !closed);
        if (!closed) {
            var total = Math.max((paste.edit_window || 600) * 1000, 1);
            var share = Math.min(Math.max(left / total, 0), 1);
            // Set through the CSSOM, which the Content-Security-Policy allows
            $state.find('.ring-left').css('stroke-dashoffset', String(100 - share * 100));
            $state.toggleClass('low', left < 60000);
            $('.window-text').text('Same link, ' + mmss(left) + ' left');
            $('.window-hint').text('Share the page link; opening /raw from another client ends editing.');
        } else {
            $state.removeClass('low');
            $('.window-text').text({
                seen: 'Opened by someone else',
                revisions: 'History full',
                expired: 'Edit window over'
            }[closed]);
            $('.window-hint').text({
                seen: 'Someone else has seen this text, so the link keeps it. Save your changes as a new version.',
                revisions: 'This paste keeps ' + paste.max_revisions + ' older versions already. Save your changes as a new version.',
                expired: 'Changes now go to a new version with its own link. The old one stays unless you delete it.'
            }[closed]);
        }
        updateSave();
    }

    function updateSave() {
        var $save = $('.d-save');
        var editable = original !== null && !$root.find('#d-text').prop('hidden');
        $save.text(closed ? 'Save as new version' : 'Save');
        $('.d-delete-prev').prop('hidden', !closed || !editable);
        $save.prop('disabled', saving || !editable || !dirty());
        $('.d-dirty').text(dirty() ? 'Unsaved changes' : '');
    }

    // ---- actions

    function sendContent(url, method) {
        return jQuery.ajax(url, {
            type: method, data: jQuery('#d-text').val(), processData: false, dataType: 'json',
            contentType: 'text/plain; charset=utf-8', headers: {'X-CSRF-Token': ui.session() ? ui.session().csrf : ''}
        });
    }

    function save() {
        if (saving || !dirty()) {
            return;
        }
        saving = true;
        updateSave();
        var mine = seq;
        if (!closed) {
            sendContent('/api/v1/pastes/' + enc(key) + '/content', 'PUT').done(function (item) {
                if (mine !== seq) {
                    return;
                }
                original = jQuery('#d-text').val();
                ui.toast('Saved under the same link, edited at ' + clock(item.edited_at) + '.');
                reload();
            }).fail(function (xhr) {
                var body = xhr.responseJSON || {};
                if (xhr.status === 409) {
                    closed = body.reason === 'seen' ? 'seen' : (body.error === 'too_many_revisions' ? 'revisions' : 'expired');
                    stopTimer();
                    tick();
                    ui.toast('Not saved: ' + (body.message || 'the edit window is over.'));
                    return;
                }
                ui.toast(ui.errorText(xhr, 'Saving failed. Your text is still here.'));
            }).always(function () {
                saving = false;
                updateSave();
            });
            return;
        }
        var drop = $('.d-delete-prev input').prop('checked');
        sendContent('/api/v1/pastes/' + enc(key) + '/versions?delete_previous=' + (drop ? '1' : '0'), 'POST').done(function (item) {
            original = jQuery('#d-text').val();
            ui.toast('Saved as version ' + item.version + (drop ? '. The previous one is deleted.' : '.'));
            ui.navigate('/dashboard/p/' + enc(item.key));
        }).fail(function (xhr) {
            ui.toast(ui.errorText(xhr, 'Saving failed. Your text is still here.'));
        }).always(function () {
            saving = false;
            updateSave();
        });
    }

    // Show the paste again after a change, keeping the editor as it is
    function reload() {
        var mine = seq;
        ui.api('GET', '/api/v1/pastes/' + enc(key)).done(function (res) {
            if (mine === seq) {
                render(res);
                updateSave();
            }
        });
    }

    function saveMeta(e) {
        e.preventDefault();
        var $f = $('.meta-form');
        var body = {
            title: $f.find('[name=title]').val(),
            language: $f.find('[name=language]').val().trim().toLowerCase(),
            visibility: $f.find('[name=visibility]:checked').val() || 'link',
            tags: $f.find('[name=tags]').val().split(',').map(function (t) {
                return t.trim().toLowerCase();
            }).filter(Boolean)
        };
        if ($f.find('[name=expiry]').val() === 'date') {
            var when = new Date($f.find('[name=expires_at]').val());
            if (isNaN(when.getTime())) {
                $('.meta-status').text('Pick the date and time it expires.');
                return;
            }
            body.expires_at = when.toISOString().replace(/\.\d{3}Z$/, 'Z');
        } else {
            body.expires_at = null;
        }
        var $b = $f.find('button[type=submit]').prop('disabled', true);
        $('.meta-status').text('Saving…');
        ui.api('PATCH', '/api/v1/pastes/' + enc(key), body).done(function () {
            $('.meta-status').text('Saved.');
            reload();
        }).fail(function (xhr) {
            $('.meta-status').text(ui.errorText(xhr, 'Saving failed.'));
        }).always(function () {
            $b.prop('disabled', false);
        });
    }

    jQuery(function () {
        $root = jQuery('#tab-detail');
        jQuery('#d-text').on('input', updateSave).on('keydown', function (e) {
            if ((e.ctrlKey || e.metaKey) && e.key === 's') {
                e.preventDefault();
                save();
            }
        });
        $root.on('click', '.d-save', save);
        $root.on('submit', '.meta-form', saveMeta);
        $root.on('change', '[name=expiry]', function () {
            var date = jQuery(this).val() === 'date';
            var $at = $root.find('[name=expires_at]').prop('hidden', !date);
            if (date && !$at.val()) {
                $at.val(localInput(new Date(Date.now() + 7 * 86400000).toISOString()));
            }
        });
        $root.on('click', '.d-copy', function () {
            var $b = jQuery(this);
            ui.copyText(window.location.origin + '/' + key, function (ok) {
                ui.toast(ok ? 'Link copied.' : 'Copy failed. Open the paste and copy its address.');
                $b.trigger('focus');
            });
        });
        $root.on('click', '.d-duplicate', function () {
            ui.api('POST', '/api/v1/pastes/' + enc(key) + '/duplicate', {}).done(function (item) {
                ui.toast('Duplicated under a new link.');
                ui.navigate('/dashboard/p/' + enc(item.key));
            }).fail(function (xhr) {
                ui.toast(ui.errorText(xhr, 'Duplicating failed. Try again.'));
            });
        });
        $root.on('click', '.d-delete', function () {
            var name = paste ? (paste.title || paste.key) : key;
            ui.confirmAction('Delete this paste?', '"' + name + '" and its link stop working at once. Its edit history goes too. This cannot be undone.',
                'Delete paste', function () {
                    ui.api('DELETE', '/api/v1/pastes/' + enc(key), {}).done(function () {
                        original = null;
                        ui.toast('Paste deleted.');
                        ui.navigate(ui.listPath());
                    }).fail(function (xhr) {
                        ui.toast(ui.errorText(xhr, 'Deleting failed. Try again.'));
                    });
                });
        });
        // Unsaved text is not lost by accident
        window.addEventListener('beforeunload', function (e) {
            if (key && dirty()) {
                e.preventDefault();
                e.returnValue = '';
            }
        });
    });

    return {open: open, close: close};
})();
