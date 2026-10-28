// Admin page, Status tab. Every value from the server is set with .text()
// only, never as HTML.
$(function () {
    var REFRESH_MS = 30000;
    var timer = null;

    function bytes(n) {
        if (n === null || n === undefined) {
            return '-';
        }
        var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
        var i = 0;
        var v = n;
        while (Math.abs(v) >= 1024 && i < units.length - 1) {
            v /= 1024;
            i++;
        }
        return (i === 0 ? String(v) : v.toFixed(v < 10 ? 2 : 1)) + ' ' + units[i];
    }

    function count(n) {
        return Number(n || 0).toLocaleString('en-US');
    }

    function duration(seconds) {
        var d = Math.floor(seconds / 86400);
        var h = Math.floor(seconds % 86400 / 3600);
        var m = Math.floor(seconds % 3600 / 60);
        if (d > 0) {
            return d + 'd ' + h + 'h';
        }
        if (h > 0) {
            return h + 'h ' + m + 'm';
        }
        return m + 'm ' + Math.floor(seconds % 60) + 's';
    }

    function when(iso) {
        var t = new Date(iso);
        return isNaN(t.getTime()) ? '-' : t.toLocaleString();
    }

    // A row: label, value, an optional note under it, an optional meter
    function row($dl, label, value, note, meter) {
        var $dd = $('<dd></dd>').text(value);
        if (note) {
            $dd.append($('<small></small>').text(note));
        }
        if (meter) {
            $dd.append(meter);
        }
        $dl.append($('<dt></dt>').text(label), $dd);
        return $dd;
    }

    function chip(text, state) {
        return $('<span class="chip"></span>').addClass(state || '').text(text);
    }

    // A bar of used out of total; warn from 70 percent, bad from 90
    function meter(used, total) {
        var share = total > 0 ? Math.min(used / total, 1) : 0;
        var $meter = $('<div class="meter" role="presentation"><span></span></div>');
        if (share >= 0.9) {
            $meter.addClass('bad');
        } else if (share >= 0.7) {
            $meter.addClass('warn');
        }
        // Set through the CSSOM, which the Content-Security-Policy allows
        $meter.children('span').css('width', (share * 100).toFixed(1) + '%');
        return $meter;
    }

    function budgetRow($dl, label, budget) {
        if (!budget || !budget.size) {
            row($dl, label, 'no budget');
            return;
        }
        row($dl, label, bytes(budget.in_use) + ' of ' + bytes(budget.size), null, meter(budget.in_use, budget.size));
    }

    function renderServer(st) {
        var $dl = $('#card-server .rows').empty();
        row($dl, 'Version', st.version);
        row($dl, 'Uptime', duration(st.uptime_seconds), 'since ' + when(st.started_at));
        row($dl, 'Storage', st.storage);
        row($dl, 'Server time', when(st.server_now));
    }

    function renderStorage(st) {
        var $dl = $('#card-storage .rows').empty();
        if (st.disk) {
            var d = st.disk;
            var $dd;
            if (d.error) {
                $dd = row($dl, 'Disk free', 'unknown');
                $dd.append(chip(d.error, 'warn'));
            } else {
                $dd = row($dl, 'Disk free', bytes(d.free),
                    d.min_free_disk > 0 ? 'uploads stop below ' + bytes(d.min_free_disk) : 'min_free_disk is off');
                if (d.min_free_disk > 0) {
                    var state = d.free <= d.min_free_disk ? ['full', 'bad'] : (d.free < 2 * d.min_free_disk ? ['low', 'warn'] : ['ok', '']);
                    $dd.contents().first().after(chip(state[0], state[1]));
                }
            }
        } else {
            row($dl, 'Disk free', 'not file storage');
        }
        if (st.file_pastes) {
            var fp = st.file_pastes;
            var $files = row($dl, 'Paste files', count(fp.count) + ' / ' + bytes(fp.bytes), 'as of ' + when(fp.as_of));
            if (fp.error) {
                $files.append(chip(fp.error, 'warn'));
            }
        }
    }

    function renderAccounts(st) {
        var $dl = $('#card-accounts .rows').empty();
        var r = st.registered;
        if (!r) {
            row($dl, 'Accounts', 'off');
            return;
        }
        if (r.error) {
            row($dl, 'Database', r.error).append(chip('error', 'bad'));
            return;
        }
        row($dl, 'Users', count(r.users));
        row($dl, 'Registered pastes', count(r.pastes) + ' / ' + bytes(r.paste_bytes));
        row($dl, 'Revisions', count(r.revisions) + ' / ' + bytes(r.revision_bytes));
        row($dl, 'Sessions', count(r.sessions));
        row($dl, 'API tokens', count(r.tokens));
    }

    function renderLoad(st) {
        var $dl = $('#card-load .rows').empty();
        budgetRow($dl, 'Read memory', st.read_budget);
        budgetRow($dl, 'Upload memory', st.upload_budget);
        var slots = st.upload_slots;
        if (slots.total > 0) {
            row($dl, 'Upload slots', slots.in_use + ' of ' + slots.total, null, meter(slots.in_use, slots.total));
        } else {
            row($dl, 'Upload slots', slots.in_use + ' in use', 'no limit');
        }
        row($dl, 'In flight', count(st.inflight.requests) + ' requests', 'from ' + count(st.inflight.clients) + ' clients');
    }

    function renderMCP(st) {
        var $dl = $('#card-mcp .rows').empty();
        var m = st.mcp;
        if (!m) {
            row($dl, 'Endpoint', 'off').append(chip('off', 'off'));
            return;
        }
        row($dl, 'In flight', count(m.in_flight) + ' requests', 'from ' + count(m.clients) + ' clients');
        row($dl, 'read_paste', m.reads_running + ' of ' + m.reads_max + ' running', null, meter(m.reads_running, m.reads_max));
        row($dl, 'search_paste', m.searches_running + ' of ' + m.searches_max + ' running', null, meter(m.searches_running, m.searches_max));
        row($dl, 'Requests', count(m.requests), count(m.refused) + ' refused for too many at once');
        row($dl, 'Tool calls', count(m.uploads) + ' uploads', count(m.reads) + ' reads, ' + count(m.searches) + ' searches');
    }

    function configValue(v) {
        if (v === null || v === undefined || v === '') {
            return '(empty)';
        }
        if (Array.isArray(v)) {
            return v.length ? v.join(', ') : '(none)';
        }
        return String(v);
    }

    function renderConfig(st) {
        var $body = $('#card-config tbody').empty();
        $.each(st.config || [], function (_, s) {
            $body.append($('<tr></tr>').append($('<td></td>').text(s.key), $('<td></td>').text(configValue(s.value))));
        });
    }

    function load() {
        var $button = $('.refresh').prop('disabled', true);
        $.ajax('/api/v1/admin/status', { dataType: 'json' }).done(function (st) {
            $('.notice').prop('hidden', true);
            renderServer(st);
            renderStorage(st);
            renderAccounts(st);
            renderLoad(st);
            renderMCP(st);
            renderConfig(st);
            $('.updated').text('Updated ' + new Date().toLocaleTimeString());
        }).fail(function (xhr) {
            var msg = xhr.status === 404 ? 'Your session ended. Log in again as an admin.' : 'The status could not be loaded. It is retried in 30 seconds.';
            $('.notice').text(msg).prop('hidden', false);
        }).always(function () {
            $button.prop('disabled', false);
        });
    }

    function schedule() {
        clearInterval(timer);
        timer = setInterval(function () {
            if (!document.hidden) {
                load();
            }
        }, REFRESH_MS);
    }

    $('.refresh').on('click', function () {
        load();
        schedule();
    });

    $.ajax('/api/v1/session', { dataType: 'json' }).done(function (s) {
        $('.who').text(s && s.user ? s.user.login : '');
    });

    load();
    schedule();
});
