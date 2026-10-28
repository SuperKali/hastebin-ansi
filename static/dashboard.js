// Dashboard placeholder: who is logged in, log in or log out.
// Text from the server is set with .text() only, never as HTML.
$(function () {
    var $status = $('.status-line');
    var session = null;

    function show(info) {
        var user = session && session.user;
        $('.signed-in').prop('hidden', !user);
        $('.signed-out').prop('hidden', !!user);
        $('.logout').prop('hidden', !user);
        $('.admin-link').prop('hidden', !(user && user.admin));
        $('.login-link').prop('hidden', !!user || !(info && info.login));
        $('.login').text(user ? user.login : '');
        $('.who').text(user ? user.login : '');
    }

    function load() {
        $.ajax('/api/info', { dataType: 'json' }).done(function (info) {
            $.ajax('/api/v1/session', { dataType: 'json' }).done(function (s) {
                session = s;
                show(info);
            }).fail(function () {
                session = null;
                show(info);
                $status.text('Could not check your login. Reload the page to try again.');
            });
        }).fail(function () {
            $status.text('The server did not answer. Reload the page to try again.');
        });
    }

    $('.logout').on('click', function () {
        var $button = $(this).prop('disabled', true);
        $.ajax('/api/v1/session/logout', {
            type: 'POST',
            contentType: 'application/json',
            data: '{}',
            headers: { 'X-CSRF-Token': session ? session.csrf : '' }
        }).done(function () {
            $status.text('You are logged out.');
            load();
        }).fail(function () {
            $status.text('Logging out failed. Reload the page and try again.');
        }).always(function () {
            $button.prop('disabled', false);
        });
    });

    load();
});
