// G5's root-dropping actor: this process itself starts as root (the
// image declares no USER), then uses gosu to run `id` as nobody, the
// pattern a real entrypoint (postgres, etc.) uses to drop root before
// serving. Proves CAP_SETUID/CAP_SETGID plus the multi-ID mapping let a
// program voluntarily become an in-sandbox uid the mapping covers but
// nothing in the image's own /etc/passwd names (nobody, 65534).
const http = require('http');
const { execSync } = require('child_process');

http.createServer((req, res) => {
  if (req.url === '/readyz') {
    res.end('ok');
    return;
  }
  const result = {};
  try {
    result.root_id = execSync('id').toString().trim();
  } catch (e) {
    result.root_id = 'id failed: ' + e.message;
  }
  try {
    result.gosu_nobody_id = execSync('gosu nobody id').toString().trim();
  } catch (e) {
    result.gosu_nobody_id = 'gosu failed: ' + e.message;
  }
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify(result) + '\n');
}).listen(8080, () => console.log('listening on 8080'));
