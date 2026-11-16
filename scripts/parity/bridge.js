const { Terminal } = require('@xterm/headless');
const rl = require('readline').createInterface({ input: process.stdin });
let term = null;
const out = (o) => process.stdout.write(JSON.stringify(o) + '\n');
rl.on('line', (line) => {
  const m = JSON.parse(line);
  if (m.op === 'open') {
    term = new Terminal({ cols: m.cols, rows: m.rows, scrollback: 5000, allowProposedApi: true });
    term.onData((d) => out({ reply: Buffer.from(d, 'binary').toString('base64') }));
    out({ ok: 'open' });
  } else if (m.op === 'write') {
    term.write(Buffer.from(m.data, 'base64'), () => out({ ok: 'write', id: m.id }));
  } else if (m.op === 'resize') {
    term.resize(m.cols, m.rows); out({ ok: 'resize' });
  } else if (m.op === 'snap') {
    const b = term.buffer.active, lines = [], all = [];
    for (let y = 0; y < b.length; y++) all.push(b.getLine(y).translateToString(true));
    for (let y = b.viewportY; y < b.viewportY + term.rows; y++) lines.push(b.getLine(y).translateToString(true));
    out({ snap: m.name, lines, all, cursor: [b.cursorY, b.cursorX], base: b.baseY });
  }
});
