// Admin page: Moderation, Users, Abuse, Status and Audit. Every value from
// the server goes in with .text() or as an attribute, never as HTML.
$(function () {
    'use strict';

    var STATUS_REFRESH_MS = 30000;
    var session = null;
    var statusTimer = null;
    var current = {
        tab: '',
        paste: null,
        users: { q: '', sort: 'last_seen', page: 1 },
        user: { id: 0, page: 1 },
        bans: 1,
        events: 1,
        audit: 1
    };

    // ---- formatting

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

    function parse(iso) {
        var t = iso ? new Date(iso) : null;
        return t && !isNaN(t.getTime()) ? t : null;
    }

    // Full date and time, for titles and the Status tab
    function when(iso) {
        var t = parse(iso);
        return t ? t.toLocaleString() : '-';
    }

    // Short: the time today, else the day, with the year when not this one
    function short(iso) {
        var t = parse(iso);
        if (!t) {
            return '-';
        }
        var now = new Date();
        if (t.toDateString() === now.toDateString()) {
            return 'today ' + t.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
        }
        var opts = { month: 'short', day: 'numeric' };
        if (t.getFullYear() !== now.getFullYear()) {
            opts.year = 'numeric';
        }
        return t.toLocaleDateString([], opts);
    }

    // How long until a time: "in 3h", "in 12m"
    function until(iso) {
        var t = parse(iso);
        if (!t) {
            return 'until lifted';
        }
        var s = Math.max(0, Math.round((t.getTime() - Date.now()) / 1000));
        if (s >= 86400 * 2) {
            return 'in ' + Math.round(s / 86400) + ' days';
        }
        if (s >= 3600) {
            return 'in ' + Math.round(s / 3600) + 'h';
        }
        return 'in ' + Math.max(1, Math.round(s / 60)) + 'm';
    }

    function timeCell(iso, fmt) {
        return $('<time></time>').attr({ datetime: iso || '', title: when(iso) }).text((fmt || short)(iso));
    }

    function plural(n, word) {
        return count(n) + ' ' + word + (n === 1 ? '' : 's');
    }

    function chip(text, state) {
        return $('<span class="chip"></span>').addClass(state || '').text(text);
    }

    // A row of a key and value list: label, value (text or nodes), a note
    function row($dl, label, value, note, extra) {
        var $dd = $('<dd></dd>');
        if (value instanceof $ || (value && value.nodeType)) {
            $dd.append(value);
        } else {
            $dd.text(value);
        }
        if (note) {
            $dd.append($('<small></small>').text(note));
        }
        if (extra) {
            $dd.append(extra);
        }
        $dl.append($('<dt></dt>').text(label), $dd);
        return $dd;
    }

    function cell(cls, label) {
        var $td = $('<td></td>').addClass(cls || '');
        if (label) {
            $td.attr('data-label', label);
        }
        return $td;
    }

    function avatar(user, size) {
        if (window.account_menu && account_menu.avatar) {
            return account_menu.avatar(user, size);
        }
        return $('<span></span>');
    }

    // Drops terminal color codes, so a log reads as text
    function plain(s) {
        return String(s).replace(/\x1b(?:\[[0-?]*[ -\/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])/g, '');
    }

    // ---- messages

    var toastTimer = null;

    function toast(msg) {
        var $t = $('.toast').removeClass('out').text(msg).prop('hidden', false);
        clearTimeout(toastTimer);
        toastTimer = setTimeout(function () {
            $t.addClass('out');
            toastTimer = setTimeout(function () {
                $t.prop('hidden', true);
            }, 300);
        }, 3200);
    }

    function notice(msg) {
        $('.notice').text(msg || '').prop('hidden', !msg);
    }

    // The path a re-login comes back to: this page, without any query
    function here() {
        return window.location.pathname;
    }

    // errorText returns what to tell about a failed call. A destructive
    // action with an old login sends the browser to GitHub and back.
    function errorText(xhr, fallback) {
        var body = xhr && xhr.responseJSON;
        if (xhr && xhr.status === 403 && body && body.error === 'reauth_required') {
            window.location.assign('/auth/github/login?reauth=1&return=' + encodeURIComponent(here()));
            return 'Confirm your login with GitHub to do this.';
        }
        if (body && body.message) {
            return body.message;
        }
        if (xhr && xhr.status === 0) {
            return 'The server could not be reached.';
        }
        return fallback || 'Something went wrong, try again.';
    }

    // ---- API

    function api(method, url, body) {
        var opts = { type: method, dataType: 'json', headers: {} };
        if (method !== 'GET') {
            opts.contentType = 'application/json';
            opts.data = JSON.stringify(body || {});
            opts.headers['X-CSRF-Token'] = (session && session.csrf) || '';
        }
        return $.ajax(url, opts);
    }

    function busy($card, on) {
        $card.toggleClass('busy', !!on).attr('aria-busy', on ? 'true' : 'false');
    }

    // ---- reason dialog

    // askReason opens the reason dialog: opts.title, opts.text, opts.ok (the
    // button), opts.danger, opts.confirm (a login to type first). done gets
    // the reason, and the typed login when asked.
    function askReason(opts, done) {
        var dialog = $('dialog.reason-dialog')[0];
        var $d = $(dialog);
        var $reason = $d.find('[name="reason"]').val('');
        var $confirm = $d.find('[name="confirm"]').val('');
        var $error = $d.find('.dialog-error').prop('hidden', true);
        $d.find('#reason-title').text(opts.title);
        $d.find('#reason-text').text(opts.text || '');
        $d.find('.confirm-field').prop('hidden', !opts.confirm);
        $d.find('.confirm-login').text(opts.confirm || '');
        $d.find('.reason-ok').text(opts.ok).toggleClass('danger', opts.danger !== false).toggleClass('primary', opts.danger === false);

        if (!dialog || typeof dialog.showModal !== 'function') {
            var r = window.prompt(opts.title + '\n\n' + (opts.text || '') + '\n\nReason:');
            if (r && r.trim()) {
                done(r.trim(), opts.confirm ? window.prompt('Type ' + opts.confirm + ' to confirm') : '');
            }
            return;
        }

        $d.find('form').off('submit').on('submit', function (evt) {
            var submitter = evt.originalEvent && evt.originalEvent.submitter;
            if (submitter && submitter.value === 'cancel') {
                return;
            }
            var reason = $.trim($reason.val());
            if (opts.confirm && $.trim($confirm.val()).toLowerCase() !== opts.confirm.toLowerCase()) {
                evt.preventDefault();
                $error.text('Type ' + opts.confirm + ' to confirm.').prop('hidden', false);
                $confirm.trigger('focus');
                return;
            }
            if (!reason) {
                evt.preventDefault();
                $error.text('Give a reason: it goes in the audit log.').prop('hidden', false);
                $reason.trigger('focus');
                return;
            }
            dialog.returnValue = 'ok';
        });
        $d.off('close').on('close', function () {
            if (dialog.returnValue === 'ok') {
                done($.trim($reason.val()), $.trim($confirm.val()));
            }
        });
        dialog.returnValue = '';
        dialog.showModal();
        (opts.confirm ? $confirm : $reason).trigger('focus');
    }

    // ---- pager

    function pager($nav, page, pages, go) {
        $nav.empty();
        if (pages <= 1) {
            $nav.prop('hidden', true);
            return;
        }
        $nav.prop('hidden', false);
        var $prev = $('<button type="button" class="btn small prev">Prev</button>').prop('disabled', page <= 1)
            .on('click', function () { go(page - 1); });
        var $next = $('<button type="button" class="btn small next">Next</button>').prop('disabled', page >= pages)
            .on('click', function () { go(page + 1); });
        $nav.append($prev, $('<span class="page-info" aria-live="polite"></span>').text('Page ' + page + ' of ' + pages), $next);
    }

    // A small menu of actions on a row: [{label, run, danger}]
    function rowMenu(label, items) {
        var $details = $('<details class="menu"></details>');
        var $summary = $('<summary class="icon-btn"></summary>').attr('aria-label', label);
        $summary.append($(document.createElementNS('http://www.w3.org/2000/svg', 'svg')).attr({
            viewBox: '0 0 16 16', width: 16, height: 16, 'aria-hidden': 'true', focusable: 'false'
        }).append(
            $(document.createElementNS('http://www.w3.org/2000/svg', 'circle')).attr({ cx: 8, cy: 3, r: 1.5, fill: 'currentColor' }),
            $(document.createElementNS('http://www.w3.org/2000/svg', 'circle')).attr({ cx: 8, cy: 8, r: 1.5, fill: 'currentColor' }),
            $(document.createElementNS('http://www.w3.org/2000/svg', 'circle')).attr({ cx: 8, cy: 13, r: 1.5, fill: 'currentColor' })
        ));
        var $list = $('<div class="menu-list"></div>');
        $.each(items, function (_, it) {
            var $b;
            if (it.href) {
                $b = $('<a></a>').attr('href', it.href).text(it.label);
                if (it.nav) {
                    $b.on('click', function (evt) {
                        if (!evt.metaKey && !evt.ctrlKey && !evt.shiftKey) {
                            evt.preventDefault();
                            $details.prop('open', false);
                            navigate(it.href);
                        }
                    });
                }
            } else {
                $b = $('<button type="button"></button>').text(it.label).on('click', function () {
                    $details.prop('open', false);
                    it.run();
                });
            }
            if (it.danger) {
                $b.addClass('m-delete');
            }
            $list.append($b);
        });
        return $details.append($summary, $list);
    }

    // Close open row menus on outside clicks and Escape
    $(document).on('click', function (evt) {
        $('details.menu[open]').each(function () {
            if (!this.contains(evt.target)) {
                this.open = false;
            }
        });
    }).on('keydown', function (evt) {
        if (evt.key === 'Escape') {
            $('details.menu[open]').each(function () {
                this.open = false;
                $(this).children('summary').trigger('focus');
            });
        }
    });

    // ---- Moderation

    function visibilityChip(vis) {
        return vis === 'private' ? chip('private', 'warn') : chip('link', 'off');
    }

    function renderPaste(p) {
        current.paste = p;
        var $card = $('.paste-card').prop('hidden', false);
        $('.mod-empty').prop('hidden', true);
        $card.find('.paste-key').text(p.key);
        var $chips = $card.find('.paste-chips').empty();
        $chips.append(p.registered ? chip('registered', '') : chip('anonymous', 'off'));
        if (p.registered) {
            $chips.append(visibilityChip(p.visibility));
        }
        if (p.tombstone) {
            $chips.append(chip('removed for abuse', 'bad'));
        } else if (!p.exists) {
            $chips.append(chip('content gone', 'warn'));
        }
        if (p.expired) {
            $chips.append(chip('expired', 'warn'));
        }
        $card.find('.paste-title').text(p.title || '').prop('hidden', !p.title);

        var $dl = $card.find('.paste-rows').empty();
        if (p.owner) {
            var $owner = $('<span class="who"></span>').append(
                avatar(p.owner, 24),
                $('<a></a>').attr('href', '/admin/users/' + p.owner.id).text(p.owner.login).on('click', function (evt) {
                    if (!evt.metaKey && !evt.ctrlKey && !evt.shiftKey) {
                        evt.preventDefault();
                        navigate('/admin/users/' + p.owner.id);
                    }
                }),
                $('<span class="muted"></span>').text('#' + p.owner.id)
            );
            if (p.owner.admin) {
                $owner.append(chip('admin', 'off'));
            }
            if (p.owner.disabled) {
                $owner.append(chip('disabled', 'bad'));
            }
            if (p.owner.abuse_strikes > 0) {
                $owner.append(chip(p.owner.abuse_strikes + (p.owner.abuse_strikes === 1 ? ' strike' : ' strikes'), 'warn'));
            }
            row($dl, 'Owner', $owner);
        } else {
            row($dl, 'Owner', 'anonymous', p.registered ? null : 'no account; only the content is stored');
        }
        row($dl, 'Size', p.exists ? bytes(p.size) : '-');
        if (p.registered) {
            row($dl, 'Created', when(p.created_at));
            row($dl, 'Expires', p.expires_at ? when(p.expires_at) : 'never');
            row($dl, 'Views', count(p.views), p.seen_at ? 'first opened by someone else ' + when(p.seen_at) : 'not opened by anyone else yet');
        } else if (p.modified_at) {
            row($dl, 'Last read or written', when(p.modified_at));
        }
        if (p.tombstone) {
            row($dl, 'Removed', when(p.tombstone.removed_at), 'reason: ' + p.tombstone.reason);
        }

        var removable = p.exists || p.registered;
        $card.find('.show-content').prop('disabled', !p.exists || !!p.tombstone);
        $card.find('.delete-paste').prop('disabled', !removable);
        $card.find('.abuse-paste').prop('disabled', !removable);
        var hint = '';
        if (!removable) {
            hint = p.tombstone ? 'This paste was removed already. Its key answers "removed by a moderator".' : 'Nothing is stored under this key now.';
        } else if (p.registered && !p.seen_at) {
            hint = 'Showing the content counts as a read by someone else: it ends the owner\'s editing under the same link.';
        }
        $card.find('.paste-hint').text(hint).prop('hidden', !hint);
    }

    function lookup(q, quiet) {
        q = $.trim(q || '');
        if (!q) {
            return;
        }
        var $card = $('.paste-card');
        $('.content-card').prop('hidden', true);
        $('.lookup .btn').prop('disabled', true);
        api('GET', '/api/v1/admin/pastes?q=' + encodeURIComponent(q)).done(function (p) {
            notice('');
            renderPaste(p);
        }).fail(function (xhr) {
            current.paste = null;
            $card.prop('hidden', true);
            var $empty = $('.mod-empty').prop('hidden', false);
            if (xhr.status === 404 && xhr.responseJSON && xhr.responseJSON.error === 'not_found') {
                $empty.find('.empty-title').text(quiet ? 'Nothing is stored under this key now' : 'No paste has this key');
                $empty.find('.empty-text').text('Check the link in the report. Expired anonymous pastes are gone for good.');
                return;
            }
            $empty.find('.empty-title').text('Look up a paste');
            $empty.find('.empty-text').text('Moderation starts from a report. Pastes are never listed or searched here.');
            notice(errorText(xhr, 'The lookup failed.'));
        }).always(function () {
            $('.lookup .btn').prop('disabled', false);
        });
    }

    $('.lookup').on('submit', function (evt) {
        evt.preventDefault();
        lookup($(this).find('[name="q"]').val());
    });

    $('.show-content').on('click', function () {
        var p = current.paste;
        if (!p) {
            return;
        }
        askReason({
            title: 'Show the content of ' + p.key,
            text: 'The first 256 KiB are shown. The view is logged with your reason' + (p.visibility === 'private' ? ', and this paste is private.' : '.'),
            ok: 'Show content',
            danger: false
        }, function (reason) {
            var $btn = $('.show-content').prop('disabled', true);
            api('POST', '/api/v1/admin/pastes/' + encodeURIComponent(p.key) + '/content', { reason: reason }).done(function (out) {
                var $c = $('.content-card').prop('hidden', false);
                $c.find('.content-note').text(out.truncated ? 'first ' + bytes(out.shown) + ' of ' + bytes(out.size) : bytes(out.size));
                $c.find('.content-view').text(plain(out.content));
                $c[0].scrollIntoView({ block: 'nearest', behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
            }).fail(function (xhr) {
                toast(errorText(xhr, 'The content could not be shown.'));
            }).always(function () {
                $btn.prop('disabled', false);
            });
        });
    });

    function removePaste(abuse) {
        var p = current.paste;
        if (!p) {
            return;
        }
        askReason(abuse ? {
            title: 'Mark ' + p.key + ' as abuse',
            text: 'The paste is deleted and its link answers "removed by a moderator" from now on' +
                (p.owner ? '. ' + p.owner.login + ' gets a strike.' : '.'),
            ok: 'Mark as abuse'
        } : {
            title: 'Delete ' + p.key,
            text: 'The paste is deleted and its link stops working. No strike, no notice on the link.',
            ok: 'Delete paste'
        }, function (reason) {
            var url = '/api/v1/admin/pastes/' + encodeURIComponent(p.key) + (abuse ? '/abuse' : '');
            api(abuse ? 'POST' : 'DELETE', url, { reason: reason }).done(function () {
                toast(abuse ? 'Marked as abuse and deleted.' : 'Paste deleted.');
                lookup(p.key, true);
            }).fail(function (xhr) {
                toast(errorText(xhr, 'The paste could not be removed.'));
            });
        });
    }

    $('.delete-paste').on('click', function () { removePaste(false); });
    $('.abuse-paste').on('click', function () { removePaste(true); });

    // ---- Users

    function stateChips(u) {
        var $s = $('<span class="chips"></span>');
        if (u.admin) {
            $s.append(chip('admin', 'off'));
        }
        if (u.disabled_at) {
            $s.append(chip('disabled', 'bad').attr('title', u.disabled_reason || ''));
        }
        if (u.abuse_strikes > 0) {
            $s.append(chip(u.abuse_strikes + (u.abuse_strikes === 1 ? ' strike' : ' strikes'), 'warn'));
        }
        if (!$s.children().length) {
            $s.append(chip('active', ''));
        }
        return $s;
    }

    // userActions returns the actions on an account, none for an admin
    function userActions(u, after) {
        if (u.admin) {
            return [];
        }
        var base = '/api/v1/admin/users/' + u.id;
        var list = [];
        if (u.disabled_at) {
            list.push({
                label: 'Enable', run: function () {
                    askReason({ title: 'Enable ' + u.login, text: 'The account works again, with its sessions and tokens.', ok: 'Enable', danger: false },
                        function (reason) { act('POST', base + '/enable', { reason: reason }, 'Account enabled.', after); });
                }
            });
        } else {
            list.push({
                label: 'Disable', danger: true, run: function () {
                    askReason({
                        title: 'Disable ' + u.login,
                        text: 'Every session and API token of this account stops working at once, uploads in progress too. Nothing is deleted.',
                        ok: 'Disable account'
                    }, function (reason) { act('POST', base + '/disable', { reason: reason }, 'Account disabled.', after); });
                }
            });
        }
        list.push({
            label: 'Revoke sessions and tokens', danger: true, run: function () {
                askReason({ title: 'Revoke everything of ' + u.login, text: 'Logs out every session and deletes every API token of this account.', ok: 'Revoke' },
                    function (reason) { act('POST', base + '/revoke', { reason: reason }, 'Sessions and tokens revoked.', after); });
            }
        });
        list.push({
            label: 'Delete account', danger: true, run: function () {
                askReason({
                    title: 'Delete ' + u.login,
                    text: 'Deletes the account with every paste, token and session. Links to its pastes stop working. This cannot be undone.',
                    ok: 'Delete account',
                    confirm: u.login
                }, function (reason, confirm) {
                    act('DELETE', base, { confirm: confirm, reason: reason }, 'Account deleted.', function () {
                        navigate('/admin/users');
                    });
                });
            }
        });
        return list;
    }

    function act(method, url, body, done, after) {
        api(method, url, body).done(function () {
            toast(done);
            if (after) {
                after();
            }
        }).fail(function (xhr) {
            toast(errorText(xhr));
        });
    }

    function userLink(u) {
        return $('<a class="title"></a>').attr('href', '/admin/users/' + u.id).text(u.login).on('click', function (evt) {
            if (!evt.metaKey && !evt.ctrlKey && !evt.shiftKey) {
                evt.preventDefault();
                navigate('/admin/users/' + u.id);
            }
        });
    }

    function loadUsers() {
        var s = current.users;
        var $card = $('.users-list-view .list-card');
        busy($card, true);
        var query = '?sort=' + encodeURIComponent(s.sort) + '&page=' + s.page + (s.q ? '&q=' + encodeURIComponent(s.q) : '');
        api('GET', '/api/v1/admin/users' + query).done(function (out) {
            notice('');
            var $body = $('table.users tbody').empty();
            $.each(out.items, function (_, u) {
                var $main = cell('c-main').append($('<div class="who"></div>').append(
                    avatar(u, 24),
                    $('<div class="who-text"></div>').append(userLink(u),
                        $('<div class="sub"></div>').text('#' + u.id + ' · GitHub ' + u.github_id))
                ));
                var actions = [{ label: 'View', href: '/admin/users/' + u.id, nav: true }].concat(userActions(u, loadUsers));
                $body.append($('<tr></tr>').toggleClass('off', !!u.disabled_at).append(
                    $main,
                    cell('num', 'pastes').text(count(u.pastes)),
                    cell('num', '').text(bytes(u.bytes)),
                    cell('c-date', 'joined').append(timeCell(u.created_at)),
                    cell('c-date', 'seen').append(timeCell(u.last_seen_at)),
                    cell('c-state').append(stateChips(u)),
                    cell('c-act').append(rowMenu('Actions for ' + u.login, actions))
                ));
            });
            $('#users-title .count').text(count(out.total));
            $('.users-list-view .empty-line').prop('hidden', out.items.length > 0);
            pager($('.users-list-view .pager'), out.page, out.pages, function (page) {
                current.users.page = page;
                loadUsers();
            });
        }).fail(function (xhr) {
            notice(errorText(xhr, 'The users could not be loaded.'));
        }).always(function () {
            busy($card, false);
        });
    }

    var searchTimer = null;
    $('.users-filter [name="q"]').on('input', function () {
        var v = $.trim($(this).val());
        clearTimeout(searchTimer);
        searchTimer = setTimeout(function () {
            current.users.q = v;
            current.users.page = 1;
            loadUsers();
        }, 300);
    });
    $('.users-filter').on('submit', function (evt) {
        evt.preventDefault();
    });
    $('.users-filter [name="sort"]').on('change', function () {
        current.users.sort = $(this).val();
        current.users.page = 1;
        loadUsers();
    });

    function loadUser() {
        var id = current.user.id;
        var $card = $('.user-pastes');
        busy($card, true);
        api('GET', '/api/v1/admin/users/' + id + '?page=' + current.user.page).done(function (u) {
            notice('');
            $('.user-avatar').empty().append(avatar(u, 40));
            $('.user-login').text(u.login);
            $('.user-ids').text('#' + u.id + ' · GitHub id ' + u.github_id + ' · joined ' + short(u.created_at));
            $('.user-chips').empty().append(stateChips(u));
            var $dl = $('.user-rows').empty();
            row($dl, 'Pastes', count(u.pastes), bytes(u.bytes) + ' with revisions');
            row($dl, 'Last seen', u.last_seen_at ? when(u.last_seen_at) : 'never');
            row($dl, 'Last login', u.last_login_at ? when(u.last_login_at) : 'never');
            row($dl, 'Ways in', plural(u.sessions, 'session') + ', ' + plural(u.tokens, 'API token'));
            if (u.disabled_at) {
                row($dl, 'Disabled', when(u.disabled_at), 'reason: ' + (u.disabled_reason || '-'));
            }
            var $actions = $('.user-actions').empty();
            var list = userActions(u, loadUser);
            if (!list.length) {
                $actions.append($('<p class="hint"></p>').text('Admin accounts cannot be changed from the admin page.'));
            }
            $.each(list, function (_, it) {
                $actions.append($('<button type="button" class="btn"></button>').addClass(it.danger ? 'danger-ghost' : '').text(it.label).on('click', it.run));
            });

            var list2 = u.paste_list;
            var $body = $('table.pastes-meta tbody').empty();
            $.each(list2.items, function (_, p) {
                var $main = cell('c-main').append(
                    $('<span class="title"></span>').text(p.title || 'untitled').toggleClass('untitled', !p.title),
                    $('<div class="sub"></div>').append($('<code></code>').text(p.key))
                );
                $body.append($('<tr></tr>').append(
                    $main,
                    cell('c-vis').append(visibilityChip(p.visibility)),
                    cell('num', '').text(bytes(p.size)),
                    cell('num', 'views').text(count(p.views)),
                    cell('c-date', 'created').append(timeCell(p.created_at)),
                    cell('c-act').append($('<button type="button" class="btn small">Look up</button>').attr('aria-label', 'Look up ' + p.key)
                        .on('click', function () {
                            navigate('/admin');
                            $('.lookup [name="q"]').val(p.key);
                            lookup(p.key);
                        }))
                ));
            });
            $('.user-pastes .count').text(count(list2.total));
            $('.user-pastes .empty-line').prop('hidden', list2.items.length > 0);
            pager($('.user-pastes .pager'), list2.page, list2.pages, function (page) {
                current.user.page = page;
                loadUser();
            });
        }).fail(function (xhr) {
            if (xhr.status === 404) {
                notice('This account does not exist any more.');
                return;
            }
            notice(errorText(xhr, 'The account could not be loaded.'));
        }).always(function () {
            busy($card, false);
        });
    }

    // ---- Abuse

    var KINDS = {
        manual: 'manual ban',
        not_found: 'key guessing',
        quota: 'upload quota',
        user_quota: 'account quota',
        not_found_ban: 'key guessing',
        no_space: 'disk full',
        inflight: 'too many at once',
        busy: 'server busy',
        rate_limit: 'rate limit'
    };

    function kindChip(kind) {
        var state = kind === 'manual' ? 'bad' : (kind === 'busy' || kind === 'no_space' ? 'off' : 'warn');
        return chip(KINDS[kind] || kind, state);
    }

    function canBan(client) {
        return /^[0-9a-fA-F:.]+$/.test(client || '');
    }

    function loadBans() {
        var $card = $('.abuse-bans');
        busy($card, true);
        api('GET', '/api/v1/admin/bans?page=' + current.bans).done(function (out) {
            var $body = $('table.bans tbody').empty();
            $.each(out.items, function (_, b) {
                var detail = b.reason || (b.kind === 'not_found' ? 'automatic, ends by itself' :
                    (b.used ? bytes(b.used) + ' stored this hour' : 'automatic, ends by itself'));
                $body.append($('<tr></tr>').append(
                    cell('c-main').append($('<code class="addr"></code>').text(b.target)),
                    cell('c-kind').append(kindChip(b.kind)),
                    cell('c-date', b.until ? 'ends' : '').append(b.until ? timeCell(b.until, until) : $('<span></span>').text('until lifted')),
                    cell('c-detail').text(detail),
                    cell('c-act').append($('<button type="button" class="btn small"></button>')
                        .text(b.kind === 'manual' ? 'Lift' : 'Reset').attr('aria-label', 'Lift the ban of ' + b.target)
                        .on('click', function () {
                            askReason({
                                title: 'Lift the ' + (KINDS[b.kind] || b.kind) + ' of ' + b.target,
                                text: b.kind === 'manual' ? 'The address can use the server again at once.' : 'The automatic block ends now; the limit counts again from zero.',
                                ok: 'Lift',
                                danger: false
                            }, function (reason) {
                                act('DELETE', '/api/v1/admin/bans/' + encodeURIComponent(b.kind) + '/' + encodeURIComponent(b.target), { reason: reason }, 'Ban lifted.', loadBans);
                            });
                        }))
                ));
            });
            $('.abuse-bans .count').text(count(out.total) + (out.truncated ? '+' : ''));
            $('.abuse-bans .empty-line').prop('hidden', out.items.length > 0);
            pager($('.abuse-bans .pager'), out.page, out.pages, function (page) {
                current.bans = page;
                loadBans();
            });
        }).fail(function (xhr) {
            notice(errorText(xhr, 'The bans could not be loaded.'));
        }).always(function () {
            busy($card, false);
        });
    }

    function loadEvents() {
        var $card = $('.abuse-events');
        busy($card, true);
        api('GET', '/api/v1/admin/events?page=' + current.events).done(function (out) {
            var $body = $('table.events tbody').empty();
            $.each(out.items, function (_, e) {
                var $act = cell('c-act');
                if (canBan(e.client)) {
                    $act.append($('<button type="button" class="btn small ghost">Ban</button>').attr('aria-label', 'Ban ' + e.client)
                        .on('click', function () {
                            var $form = $('.ban-form');
                            $form.find('[name="target"]').val(e.client);
                            $form[0].scrollIntoView({ block: 'center' });
                            $form.find('[name="reason"]').trigger('focus');
                        }));
                }
                $body.append($('<tr></tr>').append(
                    cell('c-main').append($('<code class="addr"></code>').text(e.client)),
                    cell('c-kind').append(kindChip(e.kind)),
                    cell('c-date', '').append(timeCell(e.at)),
                    cell('c-detail').text(e.detail || ''),
                    $act
                ));
            });
            $('.abuse-events .count').text(count(out.total));
            $('.abuse-events .empty-line').prop('hidden', out.items.length > 0);
            pager($('.abuse-events .pager'), out.page, out.pages, function (page) {
                current.events = page;
                loadEvents();
            });
        }).fail(function (xhr) {
            notice(errorText(xhr, 'The events could not be loaded.'));
        }).always(function () {
            busy($card, false);
        });
    }

    $('.ban-form').on('submit', function (evt) {
        evt.preventDefault();
        var $form = $(this);
        var $btn = $form.find('button').prop('disabled', true);
        api('POST', '/api/v1/admin/bans', {
            target: $.trim($form.find('[name="target"]').val()),
            reason: $.trim($form.find('[name="reason"]').val()),
            expires_in: Number($form.find('[name="expires_in"]').val())
        }).done(function (b) {
            toast(b.target + ' is banned.');
            $form.find('[name="target"], [name="reason"]').val('');
            current.bans = 1;
            loadBans();
        }).fail(function (xhr) {
            toast(errorText(xhr, 'The ban could not be added.'));
        }).always(function () {
            $btn.prop('disabled', false);
        });
    });

    $('.refresh-abuse').on('click', function () {
        loadBans();
        loadEvents();
    });

    // ---- Audit

    // The reason in the detail of an audit row, or the detail itself
    function auditReason(detail) {
        if (!detail) {
            return '';
        }
        try {
            var d = JSON.parse(detail);
            if (d && typeof d === 'object') {
                return d.reason ? String(d.reason) : '';
            }
        } catch (e) {
            // not JSON: shown as it is
        }
        return String(detail);
    }

    function loadAudit() {
        var $card = $('#tab-audit .list-card');
        busy($card, true);
        api('GET', '/api/v1/admin/audit?page=' + current.audit).done(function (out) {
            notice('');
            var $body = $('table.audit tbody').empty();
            $.each(out.items, function (_, a) {
                $body.append($('<tr></tr>').append(
                    cell('c-main').append($('<code class="action"></code>').text(a.action)),
                    cell('c-target').append($('<code></code>').text(a.target || '-')),
                    cell('c-by', 'by').text(a.actor_login || '-'),
                    cell('c-date', '').append(timeCell(a.at)),
                    cell('c-detail').text(auditReason(a.detail))
                ));
            });
            $('#audit-title .count').text(count(out.total));
            $('#tab-audit .empty-line').prop('hidden', out.items.length > 0);
            pager($('#tab-audit .pager'), out.page, out.pages, function (page) {
                current.audit = page;
                loadAudit();
            });
        }).fail(function (xhr) {
            notice(errorText(xhr, 'The audit log could not be loaded.'));
        }).always(function () {
            busy($card, false);
        });
    }

    // ---- Status

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

    function loadStatus() {
        var $button = $('.refresh').prop('disabled', true);
        api('GET', '/api/v1/admin/status').done(function (st) {
            notice('');
            renderServer(st);
            renderStorage(st);
            renderAccounts(st);
            renderLoad(st);
            renderMCP(st);
            renderConfig(st);
            $('.updated').text('Updated ' + new Date().toLocaleTimeString());
        }).fail(function (xhr) {
            notice(xhr.status === 404 ? 'Your session ended. Log in again as an admin.' : 'The status could not be loaded. It is retried in 30 seconds.');
        }).always(function () {
            $button.prop('disabled', false);
        });
    }

    function scheduleStatus(on) {
        clearInterval(statusTimer);
        statusTimer = null;
        if (on) {
            statusTimer = setInterval(function () {
                if (!document.hidden) {
                    loadStatus();
                }
            }, STATUS_REFRESH_MS);
        }
    }

    $('.refresh').on('click', function () {
        loadStatus();
        scheduleStatus(true);
    });

    // ---- routing

    function route(path) {
        var parts = path.replace(/\/+$/, '').split('/');
        // ['', 'admin', tab, id]
        var tab = parts[2] || 'moderation';
        if (tab === 'users' && /^[0-9]+$/.test(parts[3] || '')) {
            return { tab: 'users', user: Number(parts[3]) };
        }
        if (['moderation', 'users', 'abuse', 'status', 'audit'].indexOf(tab) < 0) {
            tab = 'moderation';
        }
        return { tab: tab, user: 0 };
    }

    function show(r) {
        current.tab = r.tab;
        notice('');
        $('main > .view').prop('hidden', true);
        $('#tab-' + r.tab).prop('hidden', false);
        $('.tabs a').each(function () {
            var on = $(this).data('tab') === r.tab;
            if (on) {
                $(this).attr('aria-current', 'page');
            } else {
                $(this).removeAttr('aria-current');
            }
        });
        scheduleStatus(r.tab === 'status');
        document.title = 'hastebin admin - ' + $('.tabs a[data-tab="' + r.tab + '"] span').text();

        switch (r.tab) {
            case 'moderation':
                break;
            case 'users':
                $('.users-list-view').prop('hidden', !!r.user);
                $('.user-view').prop('hidden', !r.user);
                if (r.user) {
                    if (current.user.id !== r.user) {
                        current.user = { id: r.user, page: 1 };
                    }
                    loadUser();
                } else {
                    loadUsers();
                }
                break;
            case 'abuse':
                loadBans();
                loadEvents();
                break;
            case 'status':
                loadStatus();
                break;
            case 'audit':
                loadAudit();
                break;
        }
    }

    function navigate(path) {
        if (path !== window.location.pathname) {
            history.pushState(null, '', path);
        }
        show(route(path));
        $('#main').trigger('focus');
    }

    $('.tabs a, .user-view .back').on('click', function (evt) {
        if (evt.metaKey || evt.ctrlKey || evt.shiftKey || evt.altKey || evt.button !== 0) {
            return;
        }
        evt.preventDefault();
        navigate($(this).attr('href'));
    });

    window.addEventListener('popstate', function () {
        show(route(window.location.pathname));
    });

    // ---- start

    var menu = account_menu.mount('.account-slot', { bar: 'header.top', leave: '/' });
    api('GET', '/api/v1/session').done(function (s) {
        session = s;
        menu.render(s, false);
    }).always(function () {
        $('body').removeClass('loading');
        show(route(window.location.pathname));
        if (current.tab === 'moderation') {
            $('.lookup [name="q"]').trigger('focus');
        }
    });
});
