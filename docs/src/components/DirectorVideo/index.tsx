import React, {useEffect, useState} from 'react';

type VideoStatus = {status: 'building' | 'ready' | 'failed'; video_url: string | null};
const API = 'https://director.orchael.com';

/** Renders a revision-specific Director video; refreshes automatically when ready. */
export default function DirectorVideo({id, revision}: {id: string; revision: string}): React.JSX.Element {
  const [result, setResult] = useState<VideoStatus>({status: 'building', video_url: null});
  const [error, setError] = useState(false);

  useEffect(() => {
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$/.test(id) || !(revision === "latest" || /^[a-f0-9]{40}$/.test(revision))) {
      setError(true);
      return;
    }
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    async function check() {
      try {
        const response = await fetch(`${API}/api/videos/${id}/${revision}`);
        if (!response.ok) throw new Error('status unavailable');
        const data = await response.json() as VideoStatus;
        if (!active) return;
        setResult(data);
        setError(false);
        timer = setTimeout(check, data.status === 'building' ? 5000 : 30000);
      } catch {
        if (active) {
          setError(true);
          timer = setTimeout(check, 15000);
        }
      }
    }
    setResult({status: 'building', video_url: null});
    check();
    return () => {active = false; clearTimeout(timer);};
  }, [id, revision]);

  if (result.status === 'ready' && result.video_url) {
    return <video controls preload="metadata" style={{width: '100%', borderRadius: 12}}>
      <source src={new URL(result.video_url, API).toString()} type="video/mp4" />
      Your browser does not support video playback.
    </video>;
  }
  return <div role="status" aria-live="polite" style={{
    padding: '3rem 1rem', textAlign: 'center', background: '#101827',
    color: 'white', borderRadius: 12,
  }}>
    <div style={{fontSize: 36}} aria-hidden="true">🎬</div>
    <strong>{result.status === 'failed' ? 'Video generation failed' : 'Building tutorial video'}</strong>
    <p>{error ? 'Director is temporarily unavailable. Retrying automatically…' :
      result.status === 'failed' ? 'Please check the build workflow.' :
      'This tutorial is being generated. The video will appear here automatically.'}</p>
  </div>;
}
