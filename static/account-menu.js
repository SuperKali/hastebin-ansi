// Account menu: the avatar and login of the signed-in user as a menu button,
// shared by the paste page, the dashboard and the admin page. Signed out it
// is a plain "Sign in with GitHub" link. Values from the server go in with
// .text() and attributes only, never as HTML.
var account_menu = (function () {
    'use strict';

    var SVG = 'http://www.w3.org/2000/svg';
    var AVATAR_PREFIX = 'https://avatars.githubusercontent.com/';
    var HUES = 6;
    var seq = 0;

    // Glyphs: [viewBox, path]
    var GLYPHS = {
        newPaste: ['0 0 16 16', 'M3 1.5h6.6L13 4.9V14.5H3zm1.3 1.3v10.4h7.4V6.2H8.3V2.8zm5.3.5v1.6h1.6zM7.4 7.2h1.2v1.9h1.9v1.2H8.6v1.9H7.4v-1.9H5.5V9.1h1.9z'],
        pastes: ['0 0 16 16', 'M5 1h6.5L15 4.5V13H5zm6 1.3v2.7h2.7zM7 7h6v1.1H7zm0 2.4h6v1.1H7zM2 3.5h1.5v10.9H12V16H2z'],
        tokens: ['0 0 16 16', 'M5.5 5a3.5 3.5 0 1 1 3.3 4.7L8 10.5H6.5V12H5v1.5H2v-2.6l3.6-3.6A3.5 3.5 0 0 1 5.5 5zm4 -1.5a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4z'],
        settings: ['0 0 16 16', 'M2 3.8h6.2v1.4H2zm9.8 0H14v1.4h-2.2zM8.6 2.4H10v4.2H8.6zM2 10.8h2.2v1.4H2zm5.8 0H14v1.4H7.8zM5 9.4h1.4v4.2H5z'],
        admin: ['0 0 24 24', 'M12 2l8 3.2v6.1c0 5-3.4 9.1-8 10.7-4.6-1.6-8-5.7-8-10.7V5.2zm0 2.1L6 6.5v4.8c0 3.9 2.5 7.2 6 8.6 3.5-1.4 6-4.7 6-8.6V6.5z'],
        logout: ['0 0 16 16', 'M2 2h7v1.4H3.4v9.2H9V14H2zm8.6 2.6L14 8l-3.4 3.4-1-1 1.7-1.7H6.2V7.3h5.1L9.6 5.6z'],
        github: ['0 0 16 16', 'M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z'],
        person: ['0 0 24 24', 'M12 3.8a4.2 4.2 0 1 1 0 8.4 4.2 4.2 0 0 1 0-8.4zM3.5 21c.6-4.4 4.1-7.2 8.5-7.2s7.9 2.8 8.5 7.2z'],
        caret: ['0 0 16 16', 'M3.6 5.8l1-1L8 8.2l3.4-3.4 1 1L8 10.2z']
    };

    function glyph(name, size, cls) {
        var g = GLYPHS[name];
        var svg = document.createElementNS(SVG, 'svg');
        svg.setAttribute('viewBox', g[0]);
        svg.setAttribute('width', size);
        svg.setAttribute('height', size);
        svg.setAttribute('aria-hidden', 'true');
        svg.setAttribute('focusable', 'false');
        if (cls) {
            svg.setAttribute('class', cls);
        }
        var path = document.createElementNS(SVG, 'path');
        path.setAttribute('fill', 'currentColor');
        path.setAttribute('d', g[1]);
        svg.appendChild(path);
        return svg;
    }

    // A steady color for the letter avatar, from the login
    function hue(login) {
        var h = 0;
        for (var i = 0; i < login.length; i++) {
            h = (h * 31 + login.charCodeAt(i)) >>> 0;
        }
        return h % HUES;
    }

    // The avatar address, only when it is on GitHub's avatar host; the CSP
    // would block any other anyway
    function avatarURL(user) {
        var url = user && user.avatar_url;
        return typeof url === 'string' && url.indexOf(AVATAR_PREFIX) === 0 ? url : '';
    }

    // A round avatar: the letter sits under the picture, so it shows while the
    // picture loads and stays when it fails
    function avatar(user, size) {
        var login = String(user.login || '?');
        var $wrap = $('<span class="acct-avatar"></span>').addClass('acct-avatar-' + size);
        $('<span class="acct-letter" aria-hidden="true"></span>')
            .addClass('acct-hue-' + hue(login))
            .text(login.charAt(0).toUpperCase())
            .appendTo($wrap);
        var url = avatarURL(user);
        if (!url) {
            return $wrap.addClass('acct-no-picture');
        }
        var img = document.createElement('img');
        img.alt = '';
        img.width = size;
        img.height = size;
        img.setAttribute('loading', 'lazy');
        img.setAttribute('decoding', 'async');
        img.setAttribute('referrerpolicy', 'no-referrer');
        img.addEventListener('load', function () {
            $wrap.addClass('acct-loaded');
        });
        img.addEventListener('error', function () {
            $wrap.addClass('acct-no-picture');
            $(img).remove();
        });
        img.src = url;
        $wrap.append(img);
        return $wrap;
    }

    // The path a login comes back to
    function here() {
        return window.location.pathname + window.location.search;
    }

    // Which menu item is the page shown now
    function currentItem() {
        var path = window.location.pathname.replace(/\/+$/, '');
        if (path === '/dashboard/tokens') {
            return 'tokens';
        }
        if (path === '/dashboard/account') {
            return 'settings';
        }
        if (path === '/dashboard') {
            return 'pastes';
        }
        if (path === '/admin' || path.indexOf('/admin/') === 0) {
            return 'admin';
        }
        return '';
    }

    /**
     * Menu renders the account control into slot.
     * options.compact: avatar only, for the icon bar of the paste page
     * options.bar: the bar the menu hangs from (full width under it on phones)
     * options.newPaste: function, adds "New paste" and runs it when chosen
     * options.leave: where to go after logging out, else the page reloads
     * options.onLogout: function(), replaces the reload or the leave
     */
    var Menu = function (slot, options) {
        this.$slot = $(slot);
        this.options = options || {};
        this.id = 'acct-' + (++seq);
        this.session = null;
        this.isOpen = false;
        this.keyOpened = false;
        var _this = this;
        // A press anywhere but the button and the panel closes it; on
        // phones that includes the dimmed page under the panel
        this.onDocDown = function (evt) {
            if (_this.isOpen && !_this.$button[0].contains(evt.target) && !_this.$panel[0].contains(evt.target)) {
                _this.close(false);
            }
        };
        this.onResize = function () {
            if (_this.isOpen) {
                _this.place();
            }
        };
        this.narrow = window.matchMedia('(max-width: 520px)');
    };

    // render shows the menu for session, or the sign in link when there is no
    // user and login is on, or nothing
    Menu.prototype.render = function (session, loginOn) {
        this.close(false);
        this.session = session && session.user ? session : null;
        this.$slot.empty();
        this.$root = null;
        if (this.session) {
            this.$slot.append(this.build(this.session)).prop('hidden', false);
        } else if (loginOn) {
            this.$slot.append(this.signIn()).prop('hidden', false);
        } else {
            this.$slot.prop('hidden', true);
        }
    };

    Menu.prototype.signIn = function () {
        var $a = $('<a class="acct-signin"></a>')
            .attr('href', '/auth/github/login?return=' + encodeURIComponent(here()));
        if (this.options.compact) {
            $a.addClass('acct-compact').attr('aria-label', 'Sign in with GitHub').append(glyph('person', 21));
        } else {
            $a.append(glyph('github', 16), $('<span></span>').text('Sign in with GitHub'));
        }
        return $a;
    };

    Menu.prototype.build = function (session) {
        var _this = this;
        var user = session.user;
        var login = String(user.login || '');
        var $root = this.$root = $('<div class="acct"></div>').toggleClass('acct-compact', !!this.options.compact);

        var $button = this.$button = $('<button type="button" class="acct-button"></button>').attr({
            id: this.id + '-button',
            'aria-haspopup': 'menu',
            'aria-expanded': 'false',
            'aria-controls': this.id + '-menu'
        });
        $button.append(
            avatar(user, 24),
            $('<span class="acct-login"></span>').text(login),
            $('<span class="acct-sr"></span>').text(', account menu'),
            glyph('caret', 14, 'acct-caret')
        );

        var $panel = this.$panel = $('<div class="acct-panel" hidden></div>');
        var $head = $('<div class="acct-head"></div>').append(avatar(user, 40));
        var $who = $('<div class="acct-who"></div>').appendTo($head);
        var $name = $('<span class="acct-name"></span>').text(login).appendTo($who);
        if (user.admin) {
            $name.after($('<span class="acct-badge">admin</span>'));
        }
        $('<span class="acct-via"></span>').append(glyph('github', 12), $('<span></span>').text('Signed in with GitHub')).appendTo($who);

        var $menu = this.$menu = $('<div class="acct-menu" role="menu"></div>').attr({
            id: this.id + '-menu',
            'aria-labelledby': this.id + '-button'
        });
        var current = currentItem();
        var item = function (key, href, label, icon) {
            var $a = $('<a role="menuitem" tabindex="-1"></a>').attr('href', href).addClass('acct-item acct-' + key)
                .append(glyph(icon, 16), $('<span></span>').text(label));
            if (key === current) {
                $a.attr('aria-current', 'page');
            }
            return $a.appendTo($menu);
        };
        if (typeof this.options.newPaste === 'function') {
            item('new', '/', 'New paste', 'newPaste').on('click', function (evt) {
                if (evt.metaKey || evt.ctrlKey || evt.shiftKey || evt.altKey) {
                    return;
                }
                evt.preventDefault();
                _this.close(false);
                _this.options.newPaste();
            });
        }
        item('pastes', '/dashboard', 'My pastes', 'pastes');
        item('tokens', '/dashboard/tokens', 'Tokens', 'tokens');
        item('settings', '/dashboard/account', 'Account settings', 'settings');
        if (user.admin) {
            item('admin', '/admin', 'Admin', 'admin');
        }
        $('<div role="separator" class="acct-sep"></div>').appendTo($menu);
        this.$logout = $('<button type="button" role="menuitem" tabindex="-1" class="acct-item acct-logout"></button>')
            .append(glyph('logout', 16), $('<span></span>').text('Log out'))
            .appendTo($menu);
        this.$error = $('<p class="acct-error" role="alert" hidden></p>');

        $panel.append($head, $menu, this.$error);
        $root.append($button, $panel);
        this.wire();
        return $root;
    };

    Menu.prototype.items = function () {
        return this.$menu.find('[role="menuitem"]');
    };

    Menu.prototype.wire = function () {
        var _this = this;

        this.$button.on('keydown', function (evt) {
            switch (evt.key) {
                case 'Enter':
                case ' ':
                case 'ArrowDown':
                    evt.preventDefault();
                    _this.keyOpened = evt.key !== 'ArrowDown';
                    _this.open('first');
                    break;
                case 'ArrowUp':
                    evt.preventDefault();
                    _this.open('last');
                    break;
                case 'Escape':
                    if (_this.isOpen) {
                        evt.preventDefault();
                        _this.close(true);
                    }
                    break;
            }
        });
        // The click a key makes comes on keyup, if at all: forget the key after it
        this.$button.on('keyup', function () {
            setTimeout(function () {
                _this.keyOpened = false;
            }, 0);
        });
        this.$button.on('click', function (evt) {
            // A click the keyboard made after its own keydown already opened it
            if (_this.keyOpened && evt.detail === 0) {
                _this.keyOpened = false;
                return;
            }
            _this.keyOpened = false;
            if (_this.isOpen) {
                _this.close(true);
            } else {
                _this.open('first');
            }
        });

        this.$menu.on('keydown', function (evt) {
            var $items = _this.items();
            var at = $items.index(document.activeElement);
            var to = -1;
            switch (evt.key) {
                case 'ArrowDown':
                    to = (at + 1) % $items.length;
                    break;
                case 'ArrowUp':
                    to = at <= 0 ? $items.length - 1 : at - 1;
                    break;
                case 'Home':
                case 'PageUp':
                    to = 0;
                    break;
                case 'End':
                case 'PageDown':
                    to = $items.length - 1;
                    break;
                case 'Escape':
                    evt.preventDefault();
                    evt.stopPropagation();
                    _this.close(true);
                    return;
                case 'Tab':
                    // Focus goes back to the button first, so Tab moves on from there
                    _this.close(true);
                    return;
                case ' ':
                    evt.preventDefault();
                    if (at >= 0) {
                        $items[at].click();
                    }
                    return;
                default:
                    // Type a letter to jump to the next item that starts with it
                    if (evt.key && evt.key.length === 1 && !evt.ctrlKey && !evt.metaKey && !evt.altKey) {
                        var key = evt.key.toLowerCase();
                        for (var i = 1; i <= $items.length; i++) {
                            var n = (at + i) % $items.length;
                            if ($.trim($($items[n]).text()).toLowerCase().charAt(0) === key) {
                                to = n;
                                break;
                            }
                        }
                    }
            }
            if (to >= 0) {
                evt.preventDefault();
                $items[to].focus();
            }
        });

        // Keep hover and focus on the same item
        this.$menu.on('mousemove', '[role="menuitem"]', function () {
            if (document.activeElement !== this) {
                this.focus();
            }
        });

        // Focus leaving for somewhere else on the page closes it
        this.$root.on('focusout', function (evt) {
            var next = evt.relatedTarget;
            if (_this.isOpen && next && !_this.$root[0].contains(next)) {
                _this.close(false);
            }
        });

        this.$logout.on('click', function () {
            _this.logout();
        });
    };

    Menu.prototype.open = function (focus) {
        if (!this.$root) {
            return;
        }
        if (!this.isOpen) {
            this.isOpen = true;
            this.$error.prop('hidden', true).text('');
            this.$panel.prop('hidden', false);
            this.$button.attr('aria-expanded', 'true');
            this.$root.addClass('acct-open');
            this.place();
            document.addEventListener('pointerdown', this.onDocDown, true);
            window.addEventListener('resize', this.onResize);
            this.$slot.trigger('acctopen');
        }
        var $items = this.items();
        if ($items.length) {
            $items[focus === 'last' ? $items.length - 1 : 0].focus();
        }
    };

    Menu.prototype.close = function (returnFocus) {
        if (!this.isOpen) {
            return;
        }
        this.isOpen = false;
        this.$panel.prop('hidden', true);
        this.$button.attr('aria-expanded', 'false');
        this.$root.removeClass('acct-open acct-sheet');
        document.removeEventListener('pointerdown', this.onDocDown, true);
        window.removeEventListener('resize', this.onResize);
        if (returnFocus) {
            this.$button.trigger('focus');
        }
        this.$slot.trigger('acctclose');
    };

    // place opens the panel under the right edge of the button, moved in when
    // it would leave the screen; on phones it spans the width under the bar
    Menu.prototype.place = function () {
        var panel = this.$panel[0];
        panel.style.right = '';
        if (this.narrow.matches) {
            var bar = this.options.bar ? $(this.options.bar)[0] : null;
            var bottom = (bar || this.$root[0]).getBoundingClientRect().bottom;
            this.$root[0].style.setProperty('--acct-top', Math.max(0, Math.round(bottom)) + 'px');
            this.$root.addClass('acct-sheet');
            return;
        }
        this.$root.removeClass('acct-sheet');
        var rect = panel.getBoundingClientRect();
        var margin = 8;
        var width = document.documentElement.clientWidth;
        if (rect.right > width - margin) {
            panel.style.right = Math.ceil(rect.right - (width - margin)) + 'px';
        } else if (rect.left < margin) {
            panel.style.right = Math.floor(rect.left - margin) + 'px';
        }
    };

    Menu.prototype.logout = function () {
        var _this = this;
        if (!this.session || this.$logout.prop('disabled')) {
            return;
        }
        this.$logout.prop('disabled', true).attr('aria-disabled', 'true');
        this.$error.prop('hidden', true).text('');
        var done = function () {
            if (typeof _this.options.onLogout === 'function') {
                _this.options.onLogout();
            } else if (_this.options.leave) {
                window.location.assign(_this.options.leave);
            } else {
                window.location.reload();
            }
        };
        $.ajax('/api/v1/session/logout', {
            type: 'POST',
            contentType: 'application/json',
            data: '{}',
            headers: {'X-CSRF-Token': this.session.csrf || ''}
        }).done(done).fail(function (xhr) {
            // The session had already ended: the result is the same
            if (xhr.status === 401) {
                done();
                return;
            }
            _this.$logout.prop('disabled', false).removeAttr('aria-disabled');
            _this.$error.text('Logging out failed. Try again.').prop('hidden', false);
            _this.$logout.trigger('focus');
        });
    };

    return {
        Menu: Menu,
        // mount creates a menu in slot and returns it
        mount: function (slot, options) {
            return new Menu(slot, options);
        },
        // avatar returns the round avatar of user ({login, avatar_url}) at
        // size 24 or 40, for lists of users
        avatar: avatar
    };
})();
