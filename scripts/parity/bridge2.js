// Headless xterm.js bridge that also reports per row style runs for attribute diffs
const { Terminal } = require('@xterm/headless');
const rl = require('readline').createInterface({ input: process.stdin });
let term = null;
const out = (o) => process.stdout.write(JSON.stringify(o) + '\n');
const color = (cell, fg) => {
  const isDefault = fg ? cell.isFgDefault() : cell.isBgDefault();
  if (isDefault) return 'default';
  const isRgb = fg ? cell.isFgRGB() : cell.isBgRGB();
  const v = fg ? cell.getFgColor() : cell.getBgColor();
  return isRgb ? v.toString(16).padStart(6, '0') : 'p' + v;
};
const styleRuns = (line, cols) => {
  const runs = [];
  let cur = null;
  const cell = line.getCell(0);
  for (let x = 0; x < cols; x++) {
    const c = line.getCell(x, cell);
    if (!c) break;
    const key = [color(c, true), color(c, false), !!c.isBold(), !!c.isItalic(), !!c.isUnderline(), !!c.isInverse(), !!c.isStrikethrough(), !!c.isDim()];
    const ch = c.getChars() || (c.getWidth() === 0 ? '' : ' ');
    const k = key.join(',');
    if (cur && cur.k === k) cur.t += ch;
    else { cur = { k, key, t: ch }; runs.push(cur); }
  }
  return runs.filter((r) => r.t.trim() || r.key[1] !== 'default' || r.key[5]).map((r) => [r.key, r.t]);
};
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
    const b = term.buffer.active, lines = [], all = [], styles = [];
    if (m.name !== '_') for (let y = 0; y < b.length; y++) all.push(b.getLine(y).translateToString(true));
    for (let y = b.viewportY; y < b.viewportY + term.rows; y++) {
      lines.push(b.getLine(y).translateToString(true));
      if (m.styles) styles.push(styleRuns(b.getLine(y), term.cols));
    }
    out({ snap: m.name, lines, all, styles, cursor: [b.cursorY, b.cursorX], base: b.baseY, alt: b.type === 'alternate' });
  }
});
