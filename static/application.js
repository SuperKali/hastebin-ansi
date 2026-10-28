/* global $, hljs, window, document */

///// represents a single document
var haste_document = function () {
    this.locked = false;
};

// Escapes HTML tag characters
haste_document.prototype.htmlEscape = function (s) {
    return s
        .replace(/&/g, '&amp;')
        .replace(/>/g, '&gt;')
        .replace(/</g, '&lt;')
        .replace(/"/g, '&quot;');
};

// Pastes longer than this are shown as plain text, cut to this length,
// because highlighting them would freeze the browser
haste_document.largePaste = 1 << 20;

// Returns the start of data to show for a large paste, or null for a normal one.
// truncated means the server already cut the paste, so it is large whatever its length here.
haste_document.largePreview = function (data, truncated) {
    if (!truncated && data.length <= haste_document.largePaste) {
        return null;
    }
    // End on a whole line when there is one
    var cut = data.lastIndexOf('\n', haste_document.largePaste);
    if (cut < haste_document.largePaste / 2) {
        cut = haste_document.largePaste;
    }
    // Drop ANSI color codes: shown raw they are noise and break the line height
    return data.slice(0, cut).replace(haste_document.ansiPattern, '');
};

// CSI, OSC and two byte escape sequences, plus any stray escape character
haste_document.ansiPattern = /\x1b(?:\[[0-?]*[ -\/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])?/g;

// Get this document from the server and lock it here
haste_document.prototype.load = function (key, callback, lang) {
    console.log("Loading document key", key, "lang", lang);
    var _this = this;
    $.ajax('/documents/' + key, {
        type: 'get', dataType: 'json', success: function (res) {
            console.log("Loaded success document key", key, "lang", lang);
            _this.locked = true;
            _this.key = key;
            _this.data = res.data;
            // Owner and edit marks, for the strip under the buttons
            _this.meta = {
                owner: res.owner === true, visibility: res.visibility, editable_until: res.editable_until,
                server_now: res.server_now, edited_at: res.edited_at
            };
            // A paste over 1 MiB comes cut to its first 1 MiB; /raw has the whole file
            _this.truncated = res.truncated === true;

            var preview = haste_document.largePreview(res.data, _this.truncated);
            if (preview !== null) {
                callback({
                    value: _this.htmlEscape(preview), key: key, language: lang,
                    lineCount: preview.split('\n').length, large: true,
                    size: _this.truncated ? res.size : res.data.length
                });
                return;
            }

            let final_language;
            let highlighted;
            var high;
            if (lang === 'txt') {
                console.log("Highlighting as text");
                high = {value: _this.htmlEscape(res.data)};
                final_language = high.language;
                highlighted = high.value;
            } else if (lang !== undefined && lang !== 'ans' && hljs.getLanguage(lang)) {
                console.log("Highlighting as", lang);
                high = hljs.highlight(lang, res.data);
                final_language = high.language;
                highlighted = high.value;
            } else {
                // don't guess, use ANSI (also for unknown extensions such as .log)
                console.log("Highlighting ANSI");
                final_language = "ans"
                highlighted = new Filter({}).toHtml(res.data);
                console.log("ANSI highlighted!");
            }
            //console.log("Language FINAL", final_language, "value", highlighted);
            let lineCount = res.data.split('\n').length;
            console.log("Line count", lineCount);
            callback({
                    value: highlighted, key: key, language: final_language || lang, lineCount: lineCount
                }
            );
        }, error: function () {
            callback(false);
        }
    });
};

// Save this document to the server and lock it here. With anonymous the
// paste is sent without the CSRF token and the X-Paste-* headers: an
// anonymous public paste, which the person chose after a 401.
haste_document.prototype.save = function (data, callback, anonymous) {
    if (this.locked) {
        return false;
    }
    this.data = data;
    var _this = this;
    // Guessing the language of a large paste would freeze the browser
    var preview = haste_document.largePreview(data);
    var high = preview === null ? hljs.highlightAuto(data) : {value: _this.htmlEscape(preview)};
    // Logged in, the CSRF token makes the paste yours; without it the paste is anonymous.
    // An owned paste keeps the language its link gets, for the dashboard.
    var owned = haste_document.csrf && !anonymous;
    var headers = owned ? {'X-CSRF-Token': haste_document.csrf} : {};
    if (owned && /^[a-z0-9][a-z0-9+#._-]{0,31}$/.test(high.language || '')) {
        headers['X-Paste-Language'] = high.language;
    }
    $.ajax('/documents', {
        type: 'post', data: data, dataType: 'json', contentType: 'text/plain; charset=utf-8', headers: headers, success: function (res) {
            _this.locked = true;
            _this.key = res.key;
            callback(null, {
                value: high.value, key: res.key, language: high.language,
                lineCount: (preview === null ? data : preview).split('\n').length,
                large: preview !== null, size: data.length
            });
        }, error: function (res) {
            var err;
            try {
                err = JSON.parse(res.responseText);
            } catch (e) {
                err = {message: 'Something went wrong!'};
            }
            // The login ended: nothing was saved, and the text stays here
            err.loginEnded = res.status === 401 && owned;
            callback(err);
        }
    });
};

///// represents the paste application

var haste = function (appName, options) {
    this.appName = appName;
    this.$textarea = $('textarea');
    this.$box = $('#box');
    this.$code = $('#box code');
    this.$linenos = $('#linenos');
    this.options = options;
    this.configureShortcuts();
    this.configureButtons();
    // If Twitter is disabled, hide the button
    if (!options.twitter) {
        $('#box2 .twitter').hide();
    }
};

// Set the page title - include the appName
haste.prototype.setTitle = function (ext) {
    var title = ext ? (this.appName + ' - ' + ext) : this.appName;
    document.title = title;
};

// Show a message box. The message is text, never HTML: it can come from the server.
haste.prototype.showMessage = function (msg, cls) {
    var msgBox = $('<li></li>').addClass(cls || 'info').text(msg);
    $('#messages').prepend(msgBox);
    setTimeout(function () {
        msgBox.slideUp('fast', function () {
            $(this).remove();
        });
    }, 3000);
};

// Show the light key
haste.prototype.lightKey = function () {
    this.configureKey(['new', 'save', 'mcp', 'account']);
};

// Show the full key
haste.prototype.fullKey = function () {
    this.configureKey(['new', 'duplicate', 'twitter', 'raw', 'mcp', 'account']);
};

// Set the key up for certain things to be enabled
haste.prototype.configureKey = function (enable) {
    var $this, i = 0;
    $('#box2 .function').each(function () {
        $this = $(this);
        for (i = 0; i < enable.length; i++) {
            if ($this.hasClass(enable[i])) {
                $this.addClass('enabled');
                return true;
            }
        }
        $this.removeClass('enabled');
    });
};

// Remove the current document (if there is one)
// and set up for a new one
haste.prototype.newDocument = function (hideHistory) {
    this.$box.hide();
    this.doc = new haste_document();
    if (!hideHistory) {
        window.history.pushState(null, this.appName, '/');
    }
    this.setTitle();
    this.lightKey();
    this.hideLargeNotice();
    paste_strip.hide();
    this.$textarea.val('').show('fast', function () {
        this.focus();
    });
    this.removeLineNumbers();
};

// Map of common extensions
// Note: this list does not need to include anything that IS its extension,
// due to the behavior of lookupTypeByExtension and lookupExtensionByType
// Note: optimized for lookupTypeByExtension
haste.extensionMap = {
    rb: 'ruby',
    py: 'python',
    pl: 'perl',
    php: 'php',
    scala: 'scala',
    go: 'go',
    xml: 'xml',
    html: 'xml',
    htm: 'xml',
    css: 'css',
    js: 'javascript',
    vbs: 'vbscript',
    lua: 'lua',
    pas: 'delphi',
    java: 'java',
    cpp: 'cpp',
    cc: 'cpp',
    m: 'objectivec',
    vala: 'vala',
    sql: 'sql',
    sm: 'smalltalk',
    lisp: 'lisp',
    ini: 'ini',
    diff: 'diff',
    bash: 'bash',
    sh: 'bash',
    tex: 'tex',
    erl: 'erlang',
    hs: 'haskell',
    md: 'markdown',
    txt: '',
    coffee: 'coffee',
    swift: 'swift'
};

// Look up the extension preferred for a type
// If not found, return the type itself - which we'll place as the extension
haste.prototype.lookupExtensionByType = function (type) {
    for (let key in haste.extensionMap) {
        if (Object.prototype.hasOwnProperty.call(haste.extensionMap, key) && haste.extensionMap[key] === type) {
            console.log("Found extension", key, "for type", type);
            return key;
        }
    }
    return type;
};

// Look up the type for a given extension
// If not found, return the extension - which we'll attempt to use as the type
haste.prototype.lookupTypeByExtension = function (ext) {
    // Own keys only: an extension such as "constructor" or "__proto__" must not reach Object.prototype
    let known = ext !== undefined && Object.prototype.hasOwnProperty.call(haste.extensionMap, ext);
    let result = (known && haste.extensionMap[ext]) || ext;
    console.log("Found type", result, "for extension", ext);
    return result;
};

// Add line numbers to the document
// For the specified number of lines
haste.prototype.addLineNumbers = function (lineCount) {
    var h = '';
    for (var i = 0; i < lineCount; i++) {
        h += "<a class='linenoitem' id='l" + (i + 1).toString() + "'>" + (i + 1).toString() + "</a>" + '<br/>';
    }
    $('#linenos').html(h);
};

// Remove the line numbers
haste.prototype.removeLineNumbers = function () {
    $('#linenos').html('&gt;');
};

// Tell the reader that a large paste is cut, with a link to the full text
haste.prototype.showLargeNotice = function (key, size) {
    var mb = (size / (1 << 20)).toFixed(1);
    var shown = (haste_document.largePaste / (1 << 20)).toFixed(0);
    var $notice = $('#large-notice');
    $notice.find('.large-text').text('Large paste (' + mb + ' MB): showing the first ' + shown + ' MB as plain text, without colors.');
    $notice.find('.large-raw').attr('href', '/raw/' + encodeURIComponent(key));
    $notice.prop('hidden', false);
    $(document.body).addClass('has-large-notice');
};

haste.prototype.hideLargeNotice = function () {
    $('#large-notice').prop('hidden', true);
    $(document.body).removeClass('has-large-notice');
};

// draftKey is where the text waits in sessionStorage while the person
// signs in again; the page it returns to puts it back, for the same
// account only and for draftTTL milliseconds
haste.draftKey = 'hastebin.draft';
haste.draftTTL = 30 * 60 * 1000;

// Remember whose login ended when a save was refused with 401: the account
// of the page then. Later saves go out owned again only for that account;
// another one signed in meanwhile, in another tab, is never saved to unasked.
haste.prototype.loginEndedNow = function () {
    if (this.session && this.session.user) {
        this.endedUserID = this.session.user.id;
        this.endedLogin = this.session.user.login;
    }
    this.loginEnded = true;
};

// The account signed in now when it is not the one whose login ended, else null
haste.prototype.switchedAccount = function () {
    if (!this.loginEnded || !haste_document.csrf || !this.session || !this.session.user ||
        this.endedUserID === undefined || this.session.user.id === this.endedUserID) {
        return null;
    }
    return this.session.user;
};

// Tell the person that saving failed because their login ended, with the
// two ways on: sign in again and come back to this text, or save it as an
// anonymous public paste. When another account signed in since, it names
// that account and asks before saving there instead. Nothing is saved until
// they choose, and every later save in this page shows this again until
// they do.
haste.prototype.showLoginEnded = function () {
    var _this = this;
    this.loginEnded = true;
    var $panel = $('#login-ended');
    this.renderLoginEnded();
    $panel.find('.login-ended-signin').off('click').on('click', function () {
        if (_this.switchedAccount()) {
            _this.saveToCurrentAccount();
        } else {
            _this.signInAgain();
        }
    });
    $panel.find('.login-ended-anon').off('click').on('click', function () {
        _this.hideLoginEnded();
        _this.lockDocument(true);
    });
    $panel.find('.login-ended-close').off('click').on('click', function () {
        _this.hideLoginEnded();
        _this.$textarea.focus();
    });
    $panel.off('keydown').on('keydown', function (evt) {
        if (evt.key === 'Escape') {
            _this.hideLoginEnded();
            _this.$textarea.focus();
        }
    });
    $panel.prop('hidden', false);
    $(document.body).addClass('has-login-ended');
    $panel.find('.login-ended-signin').focus();
    // The account control, and this panel, show what the server thinks now
    this.loadSession();
};

// Fill the panel for the state now: the login ended, or another account
// signed in since. Logins are set as text, never as markup.
haste.prototype.renderLoginEnded = function () {
    var $panel = $('#login-ended');
    var $title = $panel.find('#login-ended-title');
    var $text = $panel.find('#login-ended-text');
    var $signin = $panel.find('.login-ended-signin');
    $.each([$title, $text, $signin], function (i, $el) {
        if ($el.data('default') === undefined) {
            $el.data('default', $el.text());
        }
    });
    var other = this.switchedAccount();
    $panel.toggleClass('switched', !!other);
    if (!other) {
        $title.text($title.data('default'));
        $text.text($text.data('default'));
        $signin.text($signin.data('default'));
        return;
    }
    $title.text('Not saved: you are now signed in as ' + other.login + '.');
    $text.text((this.endedLogin ? this.endedLogin + "'s login ended" : 'Your login ended') +
        ' and ' + other.login + ' signed in, maybe in another tab. Save to ' + other.login +
        "'s account? Your text is still here.");
    $signin.text('Save to ' + other.login + "'s account");
};

// The person chose to save to the account signed in now
haste.prototype.saveToCurrentAccount = function () {
    this.endedUserID = undefined;
    this.endedLogin = undefined;
    this.loginEnded = false;
    this.hideLoginEnded();
    this.lockDocument();
};

haste.prototype.hideLoginEnded = function () {
    $('#login-ended').prop('hidden', true);
    $(document.body).removeClass('has-login-ended');
};

// Sign in again and come back to this page with the text. The text waits in
// sessionStorage, which only this tab sees; when the browser keeps none, the
// sign in opens in a new tab and this one keeps the text.
haste.prototype.signInAgain = function () {
    var back = window.location.pathname + window.location.search;
    var url = '/auth/github/login?return=' + encodeURIComponent(back);
    try {
        if (this.endedUserID === undefined) {
            throw new Error('no account to keep the text for');
        }
        window.sessionStorage.setItem(haste.draftKey, JSON.stringify({
            text: this.$textarea.val(), user: this.endedUserID, saved: Date.now()
        }));
    } catch (e) {
        var _this = this;
        window.open(url, '_blank', 'noopener');
        this.hideLoginEnded();
        $(window).one('focus', function () {
            _this.loadSession();
        });
        this.showMessage('Sign in in the new tab, then come back and press Save.');
        return;
    }
    window.location.href = url;
};

// Put back the text saved by signInAgain, once, on a new document: only
// when the account now signed in is the one whose login ended, and only
// within draftTTL. Any other draft is dropped unread.
haste.prototype.restoreDraft = function () {
    var draft = null;
    try {
        draft = JSON.parse(window.sessionStorage.getItem(haste.draftKey));
    } catch (e) {
        draft = null;
    }
    try {
        window.sessionStorage.removeItem(haste.draftKey);
    } catch (e) { /* nothing kept */
    }
    if (!draft || typeof draft.text !== 'string' || typeof draft.user !== 'number' ||
        typeof draft.saved !== 'number' || !(Date.now() - draft.saved < haste.draftTTL && Date.now() >= draft.saved)) {
        return;
    }
    var _this = this;
    $.ajax('/api/v1/session', {
        type: 'get', dataType: 'json', cache: false, success: function (session) {
            if (!session || !session.user || session.user.id !== draft.user) {
                return;
            }
            if (_this.doc && !_this.doc.locked && $.trim(_this.$textarea.val()) === '') {
                _this.$textarea.val(draft.text).focus();
                _this.showMessage('Your text is back. Press Save to keep it.');
            }
        }
    });
};

// Load a document and show it
haste.prototype.loadDocument = function (key) {
    console.log("LOADING DOCUMENT");
    // Split the key up
    var parts = key.split('.', 2);
    console.log("Loading document", key, "with parts", parts);
    // Ask for what we want
    var _this = this;
    _this.doc = new haste_document();
    _this.doc.load(parts[0], function (ret) {
        if (ret) {
            _this.$code.html(ret.value);
            _this.setTitle(ret.key);
            _this.fullKey();
            _this.$textarea.val('').hide();
            _this.$box.show().focus();
            _this.addLineNumbers(ret.lineCount);
            if (ret.large) {
                _this.showLargeNotice(ret.key, ret.size);
            } else {
                _this.hideLargeNotice();
            }
            paste_strip.show(_this.doc.meta, ret.key);
        } else {
            _this.newDocument();
        }
    }, this.lookupTypeByExtension(parts[1]));
};

// Duplicate the current document - only if locked
haste.prototype.duplicateDocument = function () {
    if (!this.doc.locked) {
        return;
    }
    var _this = this;
    if (!this.doc.truncated) {
        var currentData = this.doc.data;
        this.newDocument();
        this.$textarea.val(currentData);
        return;
    }
    // Only the start of a large paste was loaded: fetch the whole text
    $.ajax('/raw/' + encodeURIComponent(this.doc.key), {
        type: 'get', dataType: 'text', success: function (data) {
            _this.newDocument();
            _this.$textarea.val(data);
        }, error: function () {
            _this.showMessage('Could not load the full paste, try again.', 'error');
        }
    });
};

// Lock the current document. With anonymous it is saved as an anonymous
// public paste even when logged in.
haste.prototype.lockDocument = function (anonymous) {
    console.log("LOCKING DOCUMENT");
    var _this = this;
    // After a login ended here, no save goes out anonymous unasked: the
    // person signs in again or picks the anonymous public paste. Who is
    // signed in now decides: the same account saves as usual, another one
    // or none gets the panel, which asks first.
    if (this.loginEnded && !anonymous) {
        this.loadSession(function () {
            if (_this.loginEnded) {
                _this.showLoginEnded();
            } else {
                _this.lockDocument();
            }
        });
        return;
    }
    if (anonymous) {
        this.loginEnded = false;
    }
    this.hideLoginEnded();
    this.doc.save(this.$textarea.val(), function (err, ret) {
        if (err && err.loginEnded) {
            _this.loginEndedNow();
            _this.showLoginEnded();
        } else if (err) {
            _this.showMessage(err.message, 'error');
        } else if (ret) {
            _this.$code.html(ret.value);
            _this.setTitle(ret.key);
            var file = '/' + ret.key;
            if (ret.language) {
                file += '.' + _this.lookupExtensionByType(ret.language);
            }
            window.history.pushState(null, _this.appName + '-' + ret.key, file);
            _this.fullKey();
            _this.$textarea.val('').hide();
            _this.$box.show().focus();
            _this.addLineNumbers(ret.lineCount);
            if (ret.large) {
                _this.showLargeNotice(ret.key, ret.size);
            }
            // Saved logged in: the paste is yours, with its edit window
            if (haste_document.csrf && !anonymous) {
                paste_strip.load(ret.key);
            }
        }
    }, anonymous);
};

haste.prototype.configureButtons = function () {
    var _this = this;
    this.buttons = [{
        $where: $('#box2 .save'), label: 'Save', shortcutDescription: 'control + s', shortcut: function (evt) {
            return evt.ctrlKey && (evt.keyCode === 83);
        }, action: function () {
            if (_this.$textarea.val().replace(/^\s+|\s+$/g, '') !== '') {
                _this.lockDocument();
            }
        }
    }, {
        $where: $('#box2 .new'), label: 'New', shortcut: function (evt) {
            return evt.ctrlKey && evt.keyCode === 78;
        }, shortcutDescription: 'control + n', action: function () {
            _this.newDocument(!_this.doc.key);
        }
    }, {
        $where: $('#box2 .duplicate'), label: 'Duplicate & Edit', shortcut: function (evt) {
            return _this.doc.locked && evt.ctrlKey && evt.keyCode === 68;
        }, shortcutDescription: 'control + d', action: function () {
            _this.duplicateDocument();
        }
    }, {
        $where: $('#box2 .raw'), label: 'Just Text', shortcut: function (evt) {
            return evt.ctrlKey && evt.shiftKey && evt.keyCode === 82;
        }, shortcutDescription: 'control + shift + r', action: function () {
            window.location.href = '/raw/' + _this.doc.key;
        }
    }, {
        $where: $('#box2 .twitter'), label: 'Twitter', shortcut: function (evt) {
            return _this.options.twitter && _this.doc.locked && evt.shiftKey && evt.ctrlKey && evt.keyCode == 84;
        }, shortcutDescription: 'control + shift + t', action: function () {
            window.open('https://twitter.com/share?url=' + encodeURI(window.location.href));
        }
    }, {
        $where: $('#box2 .mcp'), label: 'AI / MCP', shortcutDescription: 'for AI assistants', action: function () {
            if (_this.mcpPanel) {
                _this.mcpPanel.open();
            }
        }
    }];
    for (var i = 0; i < this.buttons.length; i++) {
        this.configureButton(this.buttons[i]);
    }
    // The account control opens its own menu: it only shares the label
    this.accountLabel = {$where: $('#box2 .account'), label: 'Sign in', shortcutDescription: 'with GitHub'};
    this.configureLabel(this.accountLabel);
};

haste.prototype.configureButton = function (options) {
    // Handle the click action
    options.$where.click(function (evt) {
        evt.preventDefault();
        if (!options.clickDisabled && $(this).hasClass('enabled')) {
            options.action();
        }
    });
    this.configureLabel(options);
};

// Show the label of a button under the bar, on hover and on keyboard focus.
// Not while a menu of the button is open: it would sit under the menu.
haste.prototype.configureLabel = function (options) {
    var showLabel = function () {
        if ($(this).find('[aria-expanded="true"]').length) {
            return;
        }
        $('#box3 .label').text(options.label);
        $('#box3 .shortcut').text(options.shortcutDescription || '');
        $('#box3').show();
        $(this).append($('#pointer').remove().show());
    };
    options.$where.on('mouseenter', showLabel);
    options.$where.on('focusin', function (evt) {
        try {
            if (!evt.target.matches(':focus-visible')) {
                return;
            }
        } catch (e) { /* old browser without :focus-visible */
        }
        showLabel.call(this);
    });
    // Hide the label
    options.$where.on('mouseleave focusout acctopen', function () {
        $('#box3').hide();
        $('#pointer').hide();
    });
};

// Configure keyboard shortcuts for the textarea
haste.prototype.configureShortcuts = function () {
    var _this = this;
    $(document.body).keydown(function (evt) {
        var button;
        // The MCP panel handles its own keys
        if (_this.mcpPanel && _this.mcpPanel.isOpen()) {
            return;
        }
        for (var i = 0; i < _this.buttons.length; i++) {
            button = _this.buttons[i];
            if (button.shortcut && button.shortcut(evt)) {
                evt.preventDefault();
                button.action();
                return;
            }
        }
    });
};

// Ask the server which features are on, then show the MCP button if MCP is,
// and the account button if login is
haste.prototype.loadInfo = function () {
    var _this = this;
    $.ajax('/api/info', {
        type: 'get', dataType: 'json', success: function (info) {
            if (info && info.mcp) {
                _this.mcpPanel = new mcp_panel($('#box2 .mcp'));
                $('#box2 .mcp').prop('hidden', false);
                $(document.body).addClass('mcp-on');
            }
            if (info && info.login) {
                _this.loadSession();
            }
        }
    });
};

// Ask who is logged in and set the account control up for it, then run
// done, when given
haste.prototype.loadSession = function (done) {
    var _this = this;
    $.ajax('/api/v1/session', {
        type: 'get', dataType: 'json', success: function (session) {
            _this.session = session && session.user ? session : null;
            haste_document.csrf = _this.session && session.csrf ? session.csrf : null;
            // Signed in again as the account whose login ended: saves are
            // owned again. Another account stays behind the panel, which
            // asks before saving there.
            if (haste_document.csrf && !_this.switchedAccount()) {
                _this.loginEnded = false;
            }
            if (_this.loginEnded && !$('#login-ended').prop('hidden')) {
                _this.renderLoginEnded();
            }
            _this.showAccount();
            if (done) {
                done();
            }
        }, error: function () {
            _this.session = null;
            _this.showAccount();
            if (done) {
                done();
            }
        }
    });
};

// The account control: the avatar and its menu when logged in, else a
// "Sign in with GitHub" link that comes back to this page
haste.prototype.showAccount = function () {
    var _this = this;
    var $slot = $('#box2 .account');
    if (!this.accountMenu) {
        this.accountMenu = account_menu.mount($slot, {
            compact: true,
            bar: '#box2',
            newPaste: function () {
                _this.newDocument(!_this.doc.key);
            },
            onLogout: function () {
                _this.loggedOut();
            }
        });
    }
    if (this.session) {
        this.accountLabel.label = this.session.user.login;
        this.accountLabel.shortcutDescription = 'your account';
    } else {
        this.accountLabel.label = 'Sign in';
        this.accountLabel.shortcutDescription = 'with GitHub';
    }
    this.accountMenu.render(this.session, true);
    $(document.body).addClass('account-on');
};

// After logging out from the menu: reload, so nothing of the account stays
// on the page, unless that would lose a paste being written
haste.prototype.loggedOut = function () {
    if (!this.doc.locked && $.trim(this.$textarea.val()) !== '') {
        this.session = null;
        haste_document.csrf = null;
        this.showAccount();
        this.showMessage('You are logged out. Your text is still here.');
        return;
    }
    window.location.reload();
};

///// The "AI / MCP" panel: how to add this server to an AI assistant

var mcp_panel = function ($button) {
    this.$button = $button;
    this.$overlay = $('#mcp-overlay');
    this.$panel = $('#mcp-panel');
    this.$tabs = this.$panel.find('[role="tab"]');
    this.$live = this.$panel.find('.mcp-live');
    this.fillSnippets(window.location.origin);
    this.configure();
};

// Put the site address into every snippet
mcp_panel.prototype.fillSnippets = function (origin) {
    this.$panel.find('.mcp-snippet').each(function () {
        var $snippet = $(this);
        var text = $snippet.attr('data-template').split('{origin}').join(origin);
        if ($snippet.attr('data-json')) {
            text = JSON.stringify(JSON.parse(text), null, 2);
        }
        $snippet.text(text);
    });
};

mcp_panel.prototype.isOpen = function () {
    return !this.$overlay.prop('hidden');
};

mcp_panel.prototype.open = function () {
    this.$overlay.prop('hidden', false);
    this.$button.attr('aria-expanded', 'true');
    $(document.body).addClass('mcp-open');
    this.$panel.focus();
};

mcp_panel.prototype.close = function () {
    this.$overlay.prop('hidden', true);
    this.$button.attr('aria-expanded', 'false');
    $(document.body).removeClass('mcp-open');
    this.$button.focus();
};

mcp_panel.prototype.selectTab = function ($tab) {
    this.$tabs.each(function () {
        var $t = $(this);
        var selected = $t.is($tab);
        $t.attr('aria-selected', selected ? 'true' : 'false').attr('tabindex', selected ? '0' : '-1');
        $('#' + $t.attr('aria-controls')).prop('hidden', !selected);
    });
};

// Copy text to the clipboard, with a fallback for browsers without the async API
mcp_panel.prototype.copy = function (text, done) {
    if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () {
            done(true);
        }, function () {
            done(false);
        });
        return;
    }
    var $tmp = $('<textarea class="mcp-offscreen" readonly></textarea>').val(text).appendTo(this.$panel);
    $tmp[0].select();
    var ok = false;
    try {
        ok = document.execCommand('copy');
    } catch (e) {
        ok = false;
    }
    $tmp.remove();
    done(ok);
};

mcp_panel.prototype.configure = function () {
    var _this = this;

    this.$panel.find('.mcp-close').on('click', function () {
        _this.close();
    });

    // A click on the backdrop, outside the panel, closes it
    this.$overlay.on('mousedown', function (evt) {
        if (evt.target === _this.$overlay[0]) {
            _this.close();
        }
    });

    this.$tabs.on('click', function () {
        _this.selectTab($(this));
    });
    this.$tabs.on('keydown', function (evt) {
        var index = _this.$tabs.index(this);
        var next = null;
        if (evt.key === 'ArrowRight') {
            next = (index + 1) % _this.$tabs.length;
        } else if (evt.key === 'ArrowLeft') {
            next = (index - 1 + _this.$tabs.length) % _this.$tabs.length;
        } else if (evt.key === 'Home') {
            next = 0;
        } else if (evt.key === 'End') {
            next = _this.$tabs.length - 1;
        }
        if (next !== null) {
            evt.preventDefault();
            var $next = _this.$tabs.eq(next);
            _this.selectTab($next);
            $next.focus();
        }
    });

    this.$panel.find('.mcp-copy').on('click', function () {
        var $button = $(this);
        var text = $button.siblings('.mcp-snippet').text();
        _this.copy(text, function (ok) {
            $button.text(ok ? 'Copied' : 'Press Ctrl+C').toggleClass('done', ok);
            _this.$live.text(ok ? 'Copied to the clipboard' : 'Copy failed, select the text and copy it');
            clearTimeout($button.data('timer'));
            $button.data('timer', setTimeout(function () {
                $button.text('Copy').removeClass('done');
            }, 1600));
            if (!ok) {
                var range = document.createRange();
                range.selectNodeContents($button.siblings('.mcp-snippet')[0]);
                var sel = window.getSelection();
                sel.removeAllRanges();
                sel.addRange(range);
            }
        });
    });

    // Esc closes; Tab stays inside the panel
    this.$overlay.on('keydown', function (evt) {
        if (evt.key === 'Escape') {
            evt.preventDefault();
            _this.close();
            return;
        }
        if (evt.key !== 'Tab') {
            return;
        }
        var $focusable = _this.$panel.find('button, a[href], [tabindex="0"]').filter(':visible').not('[tabindex="-1"]');
        if ($focusable.length === 0) {
            return;
        }
        var first = $focusable[0];
        var last = $focusable[$focusable.length - 1];
        var active = document.activeElement;
        if (evt.shiftKey && (active === first || active === _this.$panel[0])) {
            evt.preventDefault();
            last.focus();
        } else if (!evt.shiftKey && active === last) {
            evt.preventDefault();
            first.focus();
        }
    });
};

///// Tab behavior in the textarea - 2 spaces per tab
$(function () {

    $('textarea').keydown(function (evt) {
        if (evt.keyCode === 9) {
            evt.preventDefault();
            var myValue = '  ';
            // http://stackoverflow.com/questions/946534/insert-text-into-textarea-with-jquery
            // For browsers like Internet Explorer
            if (document.selection) {
                this.focus();
                var sel = document.selection.createRange();
                sel.text = myValue;
                this.focus();
            }
            // Mozilla and Webkit
            else if (this.selectionStart || this.selectionStart == '0') {
                var startPos = this.selectionStart;
                var endPos = this.selectionEnd;
                var scrollTop = this.scrollTop;
                this.value = this.value.substring(0, startPos) + myValue + this.value.substring(endPos, this.value.length);
                this.focus();
                this.selectionStart = startPos + myValue.length;
                this.selectionEnd = startPos + myValue.length;
                this.scrollTop = scrollTop;
            } else {
                this.value += myValue;
                this.focus();
            }
        }
    });

});
