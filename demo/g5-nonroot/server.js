// G5's non-root actor: writes a file in its own home directory and
// reports `id` over HTTP, so a passing request proves the sandbox really
// ran this process as the image's own USER (node, uid 1000), not root,
// and that uid's writes against the host-backed overlay actually
// succeeded. /readyz never touches the file, so the wakeup probe itself
// does not depend on the write this build is testing.
const http = require('http');
const fs = require('fs');
const { execSync } = require('child_process');

const home = process.env.HOME || '/home/node';
const file = home + '/hello.txt';

http.createServer((req, res) => {
  if (req.url === '/readyz') {
    res.end('ok');
    return;
  }
  const result = { home };
  try {
    fs.writeFileSync(file, 'hello from the sandbox\n');
    result.write = 'ok';
  } catch (e) {
    result.write = 'failed: ' + e.message;
  }
  try {
    result.id = execSync('id').toString().trim();
  } catch (e) {
    result.id = 'id failed: ' + e.message;
  }
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify(result) + '\n');
}).listen(8080, () => console.log('listening on 8080'));
