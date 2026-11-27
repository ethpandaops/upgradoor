import { useState } from 'react';

import { useApp } from '../appContext';
import { plural } from '../lib/format';
import { usePermissions } from './Permissions';
import { Button } from './primitives';

/**
 * RefreshImages asks the controller to look every followed tag up at its
 * registry and to read the source branch of every image now, and says what
 * came of it. The hosts report a new digest with
 * their next sync, so the list is fetched again right away and shows it later.
 */
export function RefreshImages({ onDone }: { onDone?: () => void }) {
  const { api } = useApp();
  const allowed = usePermissions().refreshImages;
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');

  const refresh = () => {
    setBusy(true);
    setNote('');

    api
      .refreshImages()
      .then(res => {
        const again = res.next
          ? ` The next lookup on request is possible at ${new Date(res.next).toLocaleTimeString([], { hourCycle: 'h23' })}.`
          : '';
        const branches =
          (res.sources ?? 0) > 0
            ? ` Reading the source ${res.sources === 1 ? 'branch' : 'branches'} of ${plural(res.sources ?? 0, 'image')}.`
            : '';
        setNote(
          (res.tags > 0
            ? `Looking up ${plural(res.tags, 'tag')}; the hosts report the result with their next sync.${again}`
            : (res.recent ?? 0) > 0
              ? `Every tag was looked up within the last half minute.${again}`
              : 'No tag is followed.') + branches
        );
        onDone?.();
      })
      .catch((err: unknown) => {
        setNote(`The lookup could not be asked for: ${err instanceof Error ? err.message : String(err)}`);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  return (
    <>
      <Button
        dense
        disabled={!allowed || busy}
        title={
          allowed
            ? 'Look every followed tag up at its registry and read every source branch now, instead of at the next interval'
            : 'This login lacks the feature roll'
        }
        onClick={refresh}
      >
        Check registries now
      </Button>
      <span role="status" className="text-xs/5 text-muted">
        {note}
      </span>
    </>
  );
}
