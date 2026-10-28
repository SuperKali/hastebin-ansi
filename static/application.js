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

// Save this document to the server and lock it here
haste_document.prototype.save = function (data, callback) {
    if (this.locked) {
        return false;
    }
    this.data = data;
    var _this = this;
    $.ajax('/documents', {
        type: 'post', data: data, dataType: 'json', contentType: 'text/plain; charset=utf-8', success: function (res) {
            _this.locked = true;
            _this.key = res.key;
            // Guessing the language of a large paste would freeze the browser
            var preview = haste_document.largePreview(data);
            var high = preview === null ? hljs.highlightAuto(data) : {value: _this.htmlEscape(preview)};
            callback(null, {
                value: high.value, key: res.key, language: high.language,
                lineCount: (preview === null ? data : preview).split('\n').length,
                large: preview !== null, size: data.length
            });
        }, error: function (res) {
            try {
                callback(JSON.parse(res.responseText));
            } catch (e) {
                callback({message: 'Something went wrong!'});
            }
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

// Lock the current document
haste.prototype.lockDocument = function () {
    console.log("LOCKING DOCUMENT");
    var _this = this;
    this.doc.save(this.$textarea.val(), function (err, ret) {
        if (err) {
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
        }
    });
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
    }, this.accountButton = {
        $where: $('#box2 .account'), label: 'Log in', shortcutDescription: 'with GitHub', action: function () {
            window.location.href = _this.accountHref();
        }
    }];
    for (var i = 0; i < this.buttons.length; i++) {
        this.configureButton(this.buttons[i]);
    }
};

haste.prototype.configureButton = function (options) {
    // Handle the click action
    options.$where.click(function (evt) {
        evt.preventDefault();
        if (!options.clickDisabled && $(this).hasClass('enabled')) {
            options.action();
        }
    });
    // Show the label, on hover and on keyboard focus
    var showLabel = function () {
        $('#box3 .label').text(options.label);
        $('#box3 .shortcut').text(options.shortcutDescription || '');
        $('#box3').show();
        $(this).append($('#pointer').remove().show());
    };
    options.$where.on('mouseenter', showLabel);
    options.$where.on('focus', function () {
        try {
            if (!this.matches(':focus-visible')) {
                return;
            }
        } catch (e) { /* old browser without :focus-visible */
        }
        showLabel.call(this);
    });
    // Hide the label
    options.$where.on('mouseleave blur', function () {
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

// Where the account button goes: the dashboard when logged in, else the
// GitHub login, coming back to the page shown now
haste.prototype.accountHref = function () {
    if (this.user) {
        return '/dashboard';
    }
    return '/auth/github/login?return=' + encodeURIComponent(window.location.pathname);
};

// Ask who is logged in and set the account button up for it
haste.prototype.loadSession = function () {
    var _this = this;
    $.ajax('/api/v1/session', {
        type: 'get', dataType: 'json', success: function (session) {
            _this.user = session && session.user ? session.user : null;
            _this.showAccount();
        }, error: function () {
            _this.user = null;
            _this.showAccount();
        }
    });
};

haste.prototype.showAccount = function () {
    var $button = $('#box2 .account');
    if (this.user) {
        this.accountButton.label = this.user.login;
        this.accountButton.shortcutDescription = 'your pastes';
        $button.attr('aria-label', 'Signed in as ' + this.user.login + ', open your pastes');
        $button.find('.account-dot').prop('hidden', false);
    } else {
        this.accountButton.label = 'Log in';
        this.accountButton.shortcutDescription = 'with GitHub';
        $button.attr('aria-label', 'Log in with GitHub');
        $button.find('.account-dot').prop('hidden', true);
    }
    $button.attr('href', this.accountHref()).prop('hidden', false);
    $(document.body).addClass('account-on');
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
