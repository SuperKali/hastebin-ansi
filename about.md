# hastebin

A paste server for logs and text. It keeps ANSI colours, so terminal
output looks the way it did in your shell.

## In the browser

Type or paste your text and press Save (control + s). The address bar
then holds the link to share.

* control + n: new paste
* control + d: duplicate the paste you are viewing and edit the copy
* control + shift + r: open the plain text

## From a shell

    curl --data-binary @build.log https://<this site>/log
    dmesg | curl --data-binary @- https://<this site>/log

The server answers with the link. Use --data-binary, because -d drops
newlines.

## Links

* /<key> shows a paste with its colours
* /<key>.sh, /<key>.py, /<key>.json and so on add syntax highlighting
* /<key>.txt shows plain text
* /raw/<key> returns the plain text, handy for curl and scripts

## From your AI assistant

Claude, Cursor, VS Code and other MCP clients can upload logs and read
pastes for you. Add the /mcp address of this site as a remote MCP server
(HTTP transport). The AI / MCP button at the top right shows the setup
for each client, and /llms.txt has the same notes for agents.

## Privacy and retention

Pastes are public to anyone who has the link. Remove passwords, tokens,
API keys and other secrets before you upload. How long pastes are kept
depends on how this server is configured, and they may be removed
without notice.

## Source

https://github.com/SuperKali/hastebin-ansi, based on haste-server by
John Crepezzi (https://github.com/seejohnrun/haste-server).
