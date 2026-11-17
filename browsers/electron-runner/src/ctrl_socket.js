// Hardening for the runner's JSON-lines control socket (/run/isola/ctrl.sock).
//
// The runner (Electron) runs as root and creates the socket world-writable by
// default, so any other uid in the container could send tab commands to the
// browser. The entrypoint exports the isolauser uid/gid (ISOLA_CTRL_SOCK_UID /
// ISOLA_CTRL_SOCK_GID), which lets us tighten the mode to 0o660 and chown the
// socket to that user — after which only root and isolauser can drive it.
//
// Mode comes first, deliberately: the sandbox drops all capabilities except a
// fixed set that does not include CAP_FOWNER, so once the chown transfers
// ownership to isolauser a subsequent chmod fails with EPERM. While the
// runner still owns the file, chmod needs no capability at all.
//
// Kept in its own module (rather than inline in main.js) so it can be unit
// tested without booting Electron.

function hardenCtrlSocket(sockPath, env = process.env, fsImpl = require('fs')) {
  try {
    fsImpl.chmodSync(sockPath, 0o660);
  } catch (err) {
    console.warn(`[electron] ctrl chmod: ${err.message}`);
  }
  const uid = parseInt(env.ISOLA_CTRL_SOCK_UID || '', 10);
  const gid = parseInt(env.ISOLA_CTRL_SOCK_GID || '', 10);
  if (Number.isInteger(uid) && Number.isInteger(gid)) {
    try {
      fsImpl.chownSync(sockPath, uid, gid);
    } catch (err) {
      console.warn(`[electron] ctrl chown: ${err.message}`);
    }
  }
}

module.exports = { hardenCtrlSocket };
