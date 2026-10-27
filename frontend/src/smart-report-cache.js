const EMPTY_STATE = Object.freeze({ loading: false, report: null, error: null });

export function smartReportKey(disk) {
  return disk?.path ? JSON.stringify([disk.path, disk.serial || "", disk.model || "", disk.size || 0]) : "";
}

// Keep a snapshot for each physical disk for this page session, including errors.
// Opening a dialog or receiving a new inventory object must not query it again.
export function createSmartReportCache(readReport) {
  const entries = new Map();
  const listeners = new Set();
  const notify = () => listeners.forEach((listener) => listener());

  function get(disk) {
    return entries.get(smartReportKey(disk))?.state || EMPTY_STATE;
  }

  function load(disk, refresh = false) {
    const key = smartReportKey(disk);
    if (!key) return Promise.resolve(EMPTY_STATE);
    let entry = entries.get(key);
    if (entry?.pending) return entry.pending;
    if (entry && !refresh) return Promise.resolve(entry.state);
    if (!entry) {
      entry = { state: EMPTY_STATE, pending: null };
      entries.set(key, entry);
    }
    entry.state = { loading: true, report: entry.state.report, error: null };
    entry.pending = Promise.resolve()
      .then(() => readReport(disk))
      .then((report) => {
        entry.state = { loading: false, report, error: null };
      })
      .catch((error) => {
        entry.state = { loading: false, report: entry.state.report, error };
      })
      .then(() => {
        entry.pending = null;
        notify();
        return entry.state;
      });
    notify();
    return entry.pending;
  }

  return {
    get,
    load,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    }
  };
}
