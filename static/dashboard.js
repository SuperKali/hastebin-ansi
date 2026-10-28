// Dashboard: your pastes, API tokens and account. Every value from the
// server is set with .text(), .val() or attributes only, never as HTML.
$(function () {
    'use strict';

    var PER_PAGE = 25;
    var session = null;
    var info = null;
    var tab = null;
    var state = {q: '', visibility: '', language: '', tags: [], sort: '-created', page: 1};
    var listSeq = 0;
    var picked = {};
    var lastPage = null;
    var account = null;
    var newToken = null;
    var toastTimer = null;
    var wide = window.matchMedia('(min-width: 761px)');

    var $main = $('#main');
    var $notice = $('.notice');
    var menu = account_menu.mount('.account-slot', {bar: 'header.top', leave: '/'});

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
        return (i === 0 ? String(v) : v.toFixed(v < 10 ? 1 : 0)) + ' ' + units[i];
    }

    function count(n) {
        return Number(n || 0).toLocaleString('en-US');
    }

    var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

    function pad(n) {
        return (n < 10 ? '0' : '') + n;
    }

    // Short date: the time today, the day this year, else the day and year
    function short(iso) {
        if (!iso) {
            return '-';
        }
        var t = new Date(iso);
        if (isNaN(t.getTime())) {
            return '-';
        }
        var now = new Date();
        if (t.toDateString() === now.toDateString()) {
            return pad(t.getHours()) + ':' + pad(t.getMinutes());
        }
        var day = MONTHS[t.getMonth()] + ' ' + t.getDate();
        return t.getFullYear() === now.getFullYear() ? day : day + ', ' + t.getFullYear();
    }

    function full(iso) {
        if (!iso) {
            return '';
        }
        var t = new Date(iso);
        return isNaN(t.getTime()) ? '' : t.toLocaleString();
    }

    function plural(n, one, many) {
        return count(n) + ' ' + (n === 1 ? one : many);
    }

    // ---- messages

    function toast(text) {
        var $t = $('.toast').text(text).prop('hidden', false).removeClass('out');
        clearTimeout(toastTimer);
        toastTimer = setTimeout(function () {
            $t.addClass('out');
            toastTimer = setTimeout(function () {
                $t.prop('hidden', true);
            }, 250);
        }, 3200);
    }

    function errorText(xhr, fallback) {
        var body = xhr && xhr.responseJSON;
        if (xhr && xhr.status === 429) {
            return 'Too many requests. Wait a moment and try again.';
        }
        if (body && body.message) {
            return body.message;
        }
        if (xhr && xhr.status === 0) {
            return 'The server did not answer. Check your connection.';
        }
        return fallback || 'Something went wrong. Try again.';
    }

    function notice(text) {
        $notice.text(text || '').prop('hidden', !text);
    }

    // ---- server calls

    function api(method, url, body) {
        var opts = {type: method, dataType: 'json', headers: {}};
        if (method !== 'GET') {
            opts.contentType = 'application/json';
            opts.data = JSON.stringify(body || {});
            opts.headers['X-CSRF-Token'] = session ? session.csrf : '';
        }
        return $.ajax(url, opts).fail(function (xhr) {
            if (xhr.status === 401) {
                session = null;
                showSignedOut();
            }
        });
    }

    // ---- copy

    function copyText(text, done) {
        if (navigator.clipboard && window.isSecureContext) {
            navigator.clipboard.writeText(text).then(function () {
                done(true);
            }, function () {
                done(fallbackCopy(text));
            });
            return;
        }
        done(fallbackCopy(text));
    }

    function fallbackCopy(text) {
        var $tmp = $('<textarea class="offscreen" readonly></textarea>').val(text).appendTo(document.body);
        $tmp[0].select();
        var ok = false;
        try {
            ok = document.execCommand('copy');
        } catch (e) {
            ok = false;
        }
        $tmp.remove();
        return ok;
    }

    function flashCopied($button, ok) {
        $button.text(ok ? 'Copied' : 'Copy failed').toggleClass('done', ok);
        setTimeout(function () {
            $button.text('Copy').removeClass('done');
        }, 1600);
    }

    // ---- confirm dialog

    function confirmAction(title, text, ok, done) {
        var dialog = $('dialog.confirm')[0];
        $('#confirm-title').text(title);
        $('#confirm-text').text(text);
        $('.confirm-ok').text(ok);
        if (!dialog || typeof dialog.showModal !== 'function') {
            if (window.confirm(title + '\n\n' + text)) {
                done();
            }
            return;
        }
        $(dialog).off('close').on('close', function () {
            if (dialog.returnValue === 'ok') {
                done();
            }
        });
        dialog.returnValue = '';
        dialog.showModal();
        $(dialog).find('.btn.ghost').trigger('focus');
    }

    // ---- shell: who is logged in, which tab

    function showSignedOut() {
        $('body').removeClass('loading');
        $('.view, .tabs').prop('hidden', true);
        menu.render(null, !!(info && info.login));
        $('#signed-out').prop('hidden', false);
        $('.login-link').prop('hidden', !(info && info.login));
        $('.login-off').prop('hidden', !info || !!info.login);
    }

    function tabFromPath() {
        var path = window.location.pathname.replace(/\/+$/, '');
        if (detailKey()) {
            return 'detail';
        }
        if (path === '/dashboard/tokens') {
            return 'tokens';
        }
        if (path === '/dashboard/account') {
            return 'account';
        }
        return 'pastes';
    }

    // The key of /dashboard/p/<key>, null on any other page
    function detailKey() {
        var m = /^\/dashboard\/p\/([^\/]+)\/?$/.exec(window.location.pathname);
        return m ? decodeURIComponent(m[1]) : null;
    }

    // Go to a dashboard page without reloading. The list keeps its filters.
    function navigate(path) {
        history.pushState(null, '', path);
        showTab(tabFromPath(), true);
        window.scrollTo(0, 0);
    }

    // The address of the list as it was last shown
    function listPath() {
        var qs = query(false);
        return '/dashboard' + (qs ? '?' + qs : '');
    }

    function showTab(name, focus) {
        if (tab === 'detail' && name !== 'detail') {
            paste_detail.close();
        }
        tab = name;
        $('.view').prop('hidden', true);
        $('#tab-' + name).prop('hidden', false);
        $('.tabs a').each(function () {
            var on = $(this).data('tab') === (name === 'detail' ? 'pastes' : name);
            if (on) {
                $(this).attr('aria-current', 'page');
            } else {
                $(this).removeAttr('aria-current');
            }
        });
        document.title = {pastes: 'hastebin - your pastes', tokens: 'hastebin - API tokens', account: 'hastebin - account',
            detail: 'hastebin - paste'}[name];
        notice('');
        if (name === 'detail') {
            paste_detail.open(detailKey(), helpers);
        } else if (name === 'pastes') {
            loadFacets();
            loadList();
        } else if (name === 'tokens') {
            fillSnippets();
            loadTokens();
        } else {
            loadAccount();
        }
        if (focus) {
            $main.trigger('focus');
        }
    }

    function start() {
        $.ajax('/api/info', {dataType: 'json'}).done(function (i) {
            info = i;
            $.ajax('/api/v1/session', {dataType: 'json'}).done(function (s) {
                $('body').removeClass('loading');
                if (!s || !s.user) {
                    session = null;
                    showSignedOut();
                    return;
                }
                session = s;
                $('#signed-out').prop('hidden', true);
                $('.tabs').prop('hidden', false);
                menu.render(s, true);
                $('.confirm-login').text(s.user.login);
                readQuery();
                showTab(tabFromPath(), false);
            }).fail(function () {
                $('body').removeClass('loading');
                notice('Could not check your login. Reload the page to try again.');
            });
        }).fail(function () {
            $('body').removeClass('loading');
            notice('The server did not answer. Reload the page to try again.');
        });
    }

    $('.tabs a').on('click', function (e) {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button === 1) {
            return;
        }
        e.preventDefault();
        var name = $(this).data('tab');
        if (name === tab) {
            return;
        }
        history.pushState(null, '', $(this).attr('href'));
        showTab(name, true);
    });

    window.addEventListener('popstate', function () {
        if (session) {
            readQuery();
            showTab(tabFromPath(), false);
        }
    });

    // ---- My pastes

    // The list state lives in the address, so a reload or a shared link keeps it
    function readQuery() {
        var params = new URLSearchParams(window.location.search);
        state.q = params.get('q') || '';
        state.visibility = params.get('visibility') || '';
        state.language = params.get('language') || '';
        state.tags = params.getAll('tag').slice(0, 10);
        state.sort = params.get('sort') || '-created';
        state.page = Math.max(parseInt(params.get('page'), 10) || 1, 1);
        var $f = $('.filters-bar');
        $f.find('[name=q]').val(state.q);
        $f.find('[name=visibility]').val(state.visibility);
        $f.find('[name=sort]').val(state.sort);
        ensureOption($f.find('[name=language]'), state.language);
        $f.find('[name=language]').val(state.language);
        renderActiveTags();
    }

    function query(withDefaults) {
        var params = new URLSearchParams();
        if (state.q) {
            params.set('q', state.q);
        }
        if (state.visibility) {
            params.set('visibility', state.visibility);
        }
        if (state.language) {
            params.set('language', state.language);
        }
        state.tags.forEach(function (t) {
            params.append('tag', t);
        });
        if (state.sort !== '-created' || withDefaults) {
            params.set('sort', state.sort);
        }
        if (state.page > 1 || withDefaults) {
            params.set('page', String(state.page));
        }
        return params.toString();
    }

    function filtered() {
        return !!(state.q || state.visibility || state.language || state.tags.length);
    }

    function writeQuery() {
        var qs = query(false);
        history.replaceState(null, '', '/dashboard' + (qs ? '?' + qs : ''));
        var n = (state.visibility ? 1 : 0) + (state.language ? 1 : 0) + state.tags.length;
        $('.filter-count').text(n ? '(' + n + ')' : '');
    }

    function ensureOption($select, value) {
        if (value && !$select.find('option').filter(function () {
            return this.value === value;
        }).length) {
            $select.append($('<option></option>').val(value).text(value));
        }
    }

    function loadFacets() {
        api('GET', '/api/v1/tags').done(function (res) {
            var $lang = $('.filters-bar [name=language]');
            $lang.find('option:not(:first)').remove();
            (res.languages || []).forEach(function (l) {
                $lang.append($('<option></option>').val(l).text(l));
            });
            ensureOption($lang, state.language);
            $lang.val(state.language);
            var $tag = $('.filters-bar [name=tag]');
            $tag.find('option:not(:first)').remove();
            (res.items || []).forEach(function (t) {
                $tag.append($('<option></option>').val(t.tag).text(t.tag + ' (' + count(t.count) + ')'));
            });
            $tag.prop('disabled', !(res.items || []).length);
            $tag.val('');
        });
    }

    function renderActiveTags() {
        var $box = $('.active-tags').empty();
        state.tags.forEach(function (t) {
            var $chip = $('<button type="button" class="tag-chip removable"></button>')
                .attr('aria-label', 'Remove tag filter ' + t).data('tag', t);
            $chip.append($('<span></span>').text(t), $('<span class="x" aria-hidden="true">&times;</span>'));
            $box.append($chip);
        });
        $box.prop('hidden', !state.tags.length);
    }

    function addTag(t) {
        if (!t || state.tags.indexOf(t) >= 0 || state.tags.length >= 10) {
            return;
        }
        state.tags.push(t);
        state.page = 1;
        renderActiveTags();
        loadList();
    }

    function loadList() {
        writeQuery();
        var seq = ++listSeq;
        $('.list-card').addClass('busy').attr('aria-busy', 'true');
        api('GET', '/api/v1/pastes?' + query(true)).done(function (res) {
            if (seq !== listSeq) {
                return;
            }
            // A page past the end after deletes: go to the last one
            if (!res.items.length && res.total > 0 && state.page > res.pages) {
                state.page = res.pages;
                loadList();
                return;
            }
            renderList(res);
        }).fail(function (xhr) {
            if (seq === listSeq && xhr.status !== 401) {
                notice(errorText(xhr, 'Could not load your pastes.'));
            }
        }).always(function () {
            if (seq === listSeq) {
                $('.list-card').removeClass('busy').removeAttr('aria-busy');
            }
        });
    }

    function renderList(res) {
        lastPage = res;
        picked = {};
        notice('');
        var $body = $('table.pastes tbody').empty();
        var tpl = document.getElementById('tpl-row');
        res.items.forEach(function (p) {
            $body.append(renderRow(p, tpl));
        });
        $('#tab-pastes .count').text(res.total ? '(' + count(res.total) + ')' : '');
        var empty = !res.items.length;
        $('table.pastes').prop('hidden', empty);
        $('.empty').prop('hidden', !empty);
        $('.list-foot').prop('hidden', empty);
        if (empty) {
            var f = filtered();
            $('.empty-title').text(f ? 'No pastes match' : 'No pastes yet');
            $('.empty-text').text(f ? 'Try other words or clear the filters.'
                : 'Pastes you save while logged in, or upload with a token, show up here and never expire.');
            $('.clear-filters').prop('hidden', !f);
            $('.empty-new, .empty-tokens').prop('hidden', f);
        }
        $('.pager').prop('hidden', res.pages <= 1);
        $('.page-info').text('Page ' + count(res.page) + ' of ' + count(res.pages));
        $('.pager .prev').prop('disabled', res.page <= 1);
        $('.pager .next').prop('disabled', res.page >= res.pages);
        updatePicked();
    }

    function renderRow(p, tpl) {
        var $row = $(document.importNode(tpl.content, true).firstElementChild);
        var name = p.title || p.key;
        var view = '/' + encodeURIComponent(p.key);
        var detail = '/dashboard/p/' + encodeURIComponent(p.key);
        $row.data('key', p.key);
        $row.find('.pick').attr('aria-label', 'Select ' + name).val(p.key);
        $row.find('a.title').attr('href', detail).attr('data-nav', '1').text(name).toggleClass('untitled', !p.title);
        $row.find('.key').text(p.key).prop('hidden', !p.title);
        var $tags = $row.find('.tags');
        (p.tags || []).forEach(function (t) {
            $tags.append($('<button type="button" class="tag-chip"></button>').text(t)
                .attr('aria-label', 'Filter by tag ' + t).data('tag', t));
        });
        $row.find('.c-lang').text(p.language || '-');
        $row.find('.vis-link').prop('hidden', p.visibility !== 'link');
        $row.find('.vis-private').prop('hidden', p.visibility !== 'private');
        $row.find('.c-size').text(bytes(p.size));
        $row.find('.c-views').text(count(p.views));
        $row.find('.c-opened').text(short(p.last_opened_at)).attr('title', full(p.last_opened_at));
        $row.find('.c-created').text(short(p.created_at)).attr('title', full(p.created_at));
        var meta = [p.language || null, p.visibility, bytes(p.size), plural(p.views, 'view', 'views'), short(p.created_at)];
        $row.find('.c-meta').text(meta.filter(Boolean).join(' · '));
        $row.find('.menu summary').attr('aria-label', 'Actions for ' + name);
        $row.find('.m-view').attr('href', view);
        $row.find('.m-edit').attr('href', detail).attr('data-nav', '1');
        $row.find('.m-raw').attr('href', '/raw' + view);
        $row.find('.m-download').attr('href', '/raw' + view + '?download=1');
        return $row;
    }

    function updatePicked() {
        var n = Object.keys(picked).length;
        $('.selected-count').text('(' + n + ')');
        $('.delete-selected').prop('disabled', n === 0);
        var boxes = $('table.pastes tbody .pick').length;
        $('.pick-all').prop('checked', boxes > 0 && n === boxes).prop('indeterminate', n > 0 && n < boxes);
        $('table.pastes tbody tr').each(function () {
            $(this).toggleClass('picked', !!picked[$(this).data('key')]);
        });
    }

    function deleteKeys(keys, done) {
        api('POST', '/api/v1/pastes/delete', {keys: keys}).done(function (res) {
            var n = res.deleted.length;
            toast(n === 1 ? 'Paste deleted.' : count(n) + ' pastes deleted.');
            if (done) {
                done();
            }
            loadFacets();
            loadList();
        }).fail(function (xhr) {
            toast(errorText(xhr, 'Deleting failed. Try again.'));
        });
    }

    var searchTimer = null;
    $('.filters-bar').on('submit', function (e) {
        e.preventDefault();
    });
    $('.filters-bar [name=q]').on('input', function () {
        var v = $(this).val().trim();
        clearTimeout(searchTimer);
        searchTimer = setTimeout(function () {
            if (v !== state.q) {
                state.q = v;
                state.page = 1;
                loadList();
            }
        }, 300);
    });
    $('.filters-bar select').on('change', function () {
        var name = this.name;
        var v = $(this).val();
        if (name === 'tag') {
            addTag(v);
            $(this).val('');
            return;
        }
        state[name] = v;
        state.page = 1;
        loadList();
    });
    $('.active-tags').on('click', '.tag-chip', function () {
        var t = $(this).data('tag');
        state.tags = state.tags.filter(function (x) {
            return x !== t;
        });
        state.page = 1;
        renderActiveTags();
        loadList();
    });
    $('table.pastes').on('click', '.tags .tag-chip', function () {
        addTag($(this).data('tag'));
    });
    $('.clear-filters').on('click', function () {
        state.q = '';
        state.visibility = '';
        state.language = '';
        state.tags = [];
        state.page = 1;
        $('.filters-bar [name=q]').val('');
        $('.filters-bar [name=visibility], .filters-bar [name=language]').val('');
        renderActiveTags();
        loadList();
    });
    $('.pager .prev').on('click', function () {
        if (state.page > 1) {
            state.page--;
            loadList();
        }
    });
    $('.pager .next').on('click', function () {
        if (lastPage && state.page < lastPage.pages) {
            state.page++;
            loadList();
        }
    });
    $('table.pastes').on('change', '.pick', function () {
        if (this.checked) {
            picked[this.value] = true;
        } else {
            delete picked[this.value];
        }
        updatePicked();
    });
    $('.pick-all').on('change', function () {
        var on = this.checked;
        picked = {};
        $('table.pastes tbody .pick').each(function () {
            this.checked = on;
            if (on) {
                picked[this.value] = true;
            }
        });
        updatePicked();
    });
    $('.delete-selected').on('click', function () {
        var keys = Object.keys(picked);
        if (!keys.length) {
            return;
        }
        var n = keys.length;
        confirmAction(n === 1 ? 'Delete 1 paste?' : 'Delete ' + count(n) + ' pastes?',
            'Their links stop working at once. This cannot be undone.',
            n === 1 ? 'Delete 1 paste' : 'Delete ' + count(n) + ' pastes', function () {
                deleteKeys(keys);
            });
    });

    // Row menu: one open at a time, closed by Escape or a click elsewhere.
    // toggle does not bubble, so it is caught in the capture phase.
    document.addEventListener('toggle', function (e) {
        var menu = e.target;
        if (!menu.matches || !menu.matches('details.menu') || !menu.open) {
            return;
        }
        $('details.menu[open]').each(function () {
            if (this !== menu) {
                this.open = false;
            }
        });
        $(menu).find('.menu-list a, .menu-list button').first().trigger('focus');
    }, true);
    $(document).on('click', function (e) {
        $('details.menu[open]').each(function () {
            if (!this.contains(e.target)) {
                this.open = false;
            }
        });
    });
    $(document).on('keydown', function (e) {
        if (e.key === 'Escape') {
            $('details.menu[open]').each(function () {
                this.open = false;
                $(this).find('summary').trigger('focus');
            });
        }
    });
    $('table.pastes').on('click', '.m-copy', function () {
        var key = $(this).closest('tr').data('key');
        var $b = $(this);
        copyText(window.location.origin + '/' + key, function (ok) {
            toast(ok ? 'Link copied.' : 'Copy failed. Open the paste and copy its address.');
            $b.closest('details')[0].open = false;
        });
    });
    $('table.pastes').on('click', '.m-duplicate', function () {
        var key = $(this).closest('tr').data('key');
        $(this).closest('details')[0].open = false;
        api('POST', '/api/v1/pastes/' + encodeURIComponent(key) + '/duplicate', {}).done(function () {
            toast('Duplicated under a new link.');
            state.page = 1;
            loadList();
        }).fail(function (xhr) {
            toast(errorText(xhr, 'Duplicating failed. Try again.'));
        });
    });
    // Links inside the dashboard open without a reload
    $main.on('click', 'a[data-nav]', function (e) {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button === 1) {
            return;
        }
        e.preventDefault();
        $(this).closest('details.menu').prop('open', false);
        navigate($(this).attr('href'));
    });
    $('table.pastes').on('click', '.m-delete', function () {
        var $row = $(this).closest('tr');
        var key = $row.data('key');
        $(this).closest('details')[0].open = false;
        confirmAction('Delete this paste?', '"' + $row.find('a.title').text() + '" and its link stop working at once. This cannot be undone.',
            'Delete paste', function () {
                deleteKeys([key]);
            });
    });

    // Filters are a disclosure on narrow screens and always open on wide ones
    function syncFilters() {
        $('details.filters').prop('open', wide.matches);
    }
    if (wide.addEventListener) {
        wide.addEventListener('change', syncFilters);
    }
    syncFilters();

    // ---- Tokens

    function fillSnippets() {
        var token = newToken || '$HASTEBIN_TOKEN';
        $('#tab-tokens .snippet[data-template]').each(function () {
            // attr, not data: jQuery would parse a template that looks like JSON
            var text = $(this).attr('data-template').split('{origin}').join(window.location.origin).split('{token}').join(token);
            if ($(this).attr('data-json') === 'true') {
                try {
                    text = JSON.stringify(JSON.parse(text), null, 2);
                } catch (e) {
                    // keep it on one line
                }
            }
            $(this).text(text);
        });
        $('.use-hint').prop('hidden', !!newToken);
    }

    function loadTokens() {
        api('GET', '/api/v1/tokens').done(function (res) {
            var $body = $('table.tokens tbody').empty();
            var tpl = document.getElementById('tpl-token');
            res.items.forEach(function (t) {
                var $row = $(document.importNode(tpl.content, true).firstElementChild);
                $row.data('id', t.id).data('name', t.name);
                $row.find('.t-name').text(t.name);
                $row.find('.t-prefix code').text(t.prefix + '…');
                $row.find('.t-created').text(short(t.created_at)).attr('title', full(t.created_at));
                $row.find('.t-used').text(t.last_used_at ? short(t.last_used_at) : 'never').attr('title', full(t.last_used_at));
                $row.find('.revoke').attr('aria-label', 'Revoke ' + t.name);
                $body.append($row);
            });
            $('table.tokens').prop('hidden', !res.items.length);
            $('.tokens-empty').prop('hidden', !!res.items.length);
            $('#tab-tokens .count').text('(' + res.items.length + ' of ' + res.max + ')');
            var full_ = res.items.length >= res.max;
            $('.create-token button, .create-token input').prop('disabled', full_);
            $('.create-token input').attr('placeholder', full_ ? 'Revoke a token to create another' : 'laptop, CI, Claude Code');
        }).fail(function (xhr) {
            if (xhr.status !== 401) {
                notice(errorText(xhr, 'Could not load your tokens.'));
            }
        });
    }

    $('.create-token').on('submit', function (e) {
        e.preventDefault();
        var $form = $(this);
        var $b = $form.find('button').prop('disabled', true);
        var name = $form.find('[name=name]').val().trim();
        api('POST', '/api/v1/tokens', {name: name}).done(function (res) {
            newToken = res.token;
            $('.token-value').text(res.token);
            $('.token-once').prop('hidden', false).trigger('focus');
            $form[0].reset();
            fillSnippets();
            loadTokens();
        }).fail(function (xhr) {
            toast(errorText(xhr, 'Creating the token failed.'));
        }).always(function () {
            $b.prop('disabled', false);
        });
    });

    $('.once-done').on('click', function () {
        newToken = null;
        $('.token-value').text('');
        $('.token-once').prop('hidden', true);
        fillSnippets();
        $('.create-token input').trigger('focus');
    });

    $('#tab-tokens').on('click', '.copy', function () {
        var $b = $(this);
        copyText($b.siblings('.snippet').text(), function (ok) {
            flashCopied($b, ok);
        });
    });

    $('table.tokens').on('click', '.revoke', function () {
        var $row = $(this).closest('tr');
        var id = $row.data('id');
        confirmAction('Revoke this token?', '"' + $row.data('name') + '" stops working at once, also for uploads in progress.',
            'Revoke token', function () {
                api('DELETE', '/api/v1/tokens/' + encodeURIComponent(id), {}).done(function () {
                    toast('Token revoked.');
                    loadTokens();
                }).fail(function (xhr) {
                    toast(errorText(xhr, 'Revoking failed.'));
                });
            });
    });

    $('.seg [role=tab]').on('click', function () {
        selectUseTab($(this));
    }).on('keydown', function (e) {
        var $tabs = $('.seg [role=tab]');
        var i = $tabs.index(this);
        if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
            e.preventDefault();
            var next = $tabs.eq((i + (e.key === 'ArrowRight' ? 1 : $tabs.length - 1)) % $tabs.length);
            selectUseTab(next);
            next.trigger('focus');
        }
    });

    function selectUseTab($t) {
        $('.seg [role=tab]').attr('aria-selected', 'false').attr('tabindex', '-1');
        $t.attr('aria-selected', 'true').removeAttr('tabindex');
        $('.use-pane').prop('hidden', true);
        $('#' + $t.attr('aria-controls')).prop('hidden', false);
    }

    // ---- Account

    function row($dl, label, value, note, meter) {
        var $dd = $('<dd></dd>').text(value);
        if (note) {
            $dd.append($('<small></small>').text(note));
        }
        if (meter) {
            $dd.append(meter);
        }
        $dl.append($('<dt></dt>').text(label), $dd);
    }

    function meter(used, total) {
        var share = total > 0 ? Math.min(used / total, 1) : 0;
        var $m = $('<div class="meter" role="presentation"><span></span></div>');
        if (share >= 0.9) {
            $m.addClass('bad');
        } else if (share >= 0.7) {
            $m.addClass('warn');
        }
        // Set through the CSSOM, which the Content-Security-Policy allows
        $m.children('span').css('width', (share * 100).toFixed(1) + '%');
        return $m;
    }

    function ofLimit(used, limit, fmt) {
        return limit > 0 ? fmt(used) + ' of ' + fmt(limit) : fmt(used) + ' (no limit)';
    }

    function loadAccount() {
        api('GET', '/api/v1/account').done(function (a) {
            account = a;
            var $p = $('.profile').empty();
            row($p, 'Login', a.login);
            row($p, 'GitHub id', String(a.github_id));
            row($p, 'Member since', full(a.created_at));
            row($p, 'Last login', a.last_login_at ? full(a.last_login_at) : '-');
            var $u = $('.usage').empty();
            var l = a.limits;
            row($u, 'Pastes', ofLimit(a.usage.pastes, l.max_pastes, count), null,
                l.max_pastes > 0 ? meter(a.usage.pastes, l.max_pastes) : null);
            row($u, 'Storage', ofLimit(a.usage.bytes, l.storage, bytes), null,
                l.storage > 0 ? meter(a.usage.bytes, l.storage) : null);
            row($u, 'Tokens', ofLimit(a.usage.tokens, l.max_tokens, count));
            row($u, 'Largest paste', l.max_length > 0 ? bytes(l.max_length) : 'no limit');
            row($u, 'Uploads per hour', l.upload_quota > 0 ? bytes(l.upload_quota) : 'no limit');
            $('.default-visibility input[value=' + (a.default_visibility === 'private' ? 'private' : 'link') + ']').prop('checked', true);
            $('.confirm-login').text(a.login);
        }).fail(function (xhr) {
            if (xhr.status !== 401) {
                notice(errorText(xhr, 'Could not load your account.'));
            }
        });
    }

    $('.default-visibility input').on('change', function () {
        var v = this.value;
        var $status = $('.visibility-status').text('Saving…');
        api('PATCH', '/api/v1/account', {default_visibility: v}).done(function (a) {
            account = a;
            $status.text(v === 'private' ? 'Saved. New pastes are private.' : 'Saved. New pastes open with their link.');
        }).fail(function (xhr) {
            $status.text(errorText(xhr, 'Saving failed.'));
            if (account) {
                $('.default-visibility input[value=' + account.default_visibility + ']').prop('checked', true);
            }
        });
    });

    $('.logout-all').on('click', function () {
        var revoke = $('.revoke-tokens').prop('checked');
        confirmAction('Log out everywhere?', revoke
            ? 'Every browser is logged out and every API token stops working.'
            : 'Every browser is logged out, this one too. API tokens keep working.',
        'Log out everywhere', function () {
            api('POST', '/api/v1/session/logout-all', {revoke_tokens: revoke}).done(function () {
                window.location.href = '/';
            }).fail(function (xhr) {
                toast(errorText(xhr, 'Logging out failed.'));
            });
        });
    });

    $('.delete-account [name=confirm]').on('input', function () {
        var login = session && session.user ? session.user.login : '';
        var match = !!login && $(this).val().trim().toLowerCase() === login.toLowerCase();
        $('.delete-account button').prop('disabled', !match);
    });

    $('.delete-account').on('submit', function (e) {
        e.preventDefault();
        var typed = $(this).find('[name=confirm]').val().trim();
        var n = account ? account.usage.pastes : 0;
        confirmAction('Delete your account?', 'Your account, ' + plural(n, 'paste', 'pastes') + ' and every token go now. This cannot be undone.',
            'Delete everything', function () {
                var $b = $('.delete-account button').prop('disabled', true);
                api('DELETE', '/api/v1/account', {confirm: typed}).done(function () {
                    window.location.href = '/';
                }).fail(function (xhr) {
                    toast(errorText(xhr, 'Deleting the account failed.'));
                    $b.prop('disabled', false);
                });
            });
    });

    // What the paste page (paste-detail.js) shares with the rest of the dashboard
    var helpers = {
        api: api, toast: toast, notice: notice, errorText: errorText, confirmAction: confirmAction,
        copyText: copyText, bytes: bytes, count: count, short: short, full: full, plural: plural,
        navigate: navigate, listPath: listPath,
        session: function () {
            return session;
        }
    };

    start();
});
