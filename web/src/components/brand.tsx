/** A little landscape cut from a notebook, with lettering drawn in pencil. */
export function Brand() {
  return (
    <div className="brand" role="img" aria-label="Dune · Preview">
      <svg className="brand-mark" viewBox="0 0 80 80" fill="none" aria-hidden="true" focusable="false">
        <path className="brand-paper" d="m9 12 17-3 18 1 22-1 5 18-1 20 2 20-19 4-22-2-21 1-3-21 2-18Z" />
        <path className="brand-paper-edge" d="m12 24-1 23 2 19 17-1m28 2 10-2-1-16" />
        <path className="brand-tape" d="m27 5 7 1 5-2 6 2 8-1-1 13-7-1-6 2-6-2-7 1Z" />
        <path className="brand-sun" d="M55 26c7-1 10 5 8 10-2 6-10 7-14 2-4-5-1-11 6-12Z" />
        <path className="brand-pencil" d="m54 21-1-3m13 7 3-2m-1 13 3 1M46 25l-2-2" />
        <path className="brand-dune-back" d="M12 49c9-2 15-16 25-13 11 3 13 17 31 13l-1 16-20 3-19-2-15 1Z" />
        <path className="brand-dune-front" d="M12 57c12 5 22 2 31-6 8-7 17-9 25-6l-1 20-19 3-19-2-16 1Z" />
        <path className="brand-pencil" d="M14 49c9-3 15-16 24-12 5 2 9 7 12 10M14 58c13 4 23-1 31-8 7-6 14-8 21-5" />
        <path className="brand-pencil brand-pencil-light" d="m21 47 5-5m-2 8 7-7m20 14 8-5m-4 7 6-4M20 62l8 1" />
        <path className="brand-pencil" d="m36 59-1-7m1 4 4-3m-5 2-3-2" />
      </svg>
      <span className="brand-lettering" aria-hidden="true">
        <svg className="brand-wordmark" viewBox="0 0 126 48" fill="none" focusable="false">
          <g className="brand-letters">
            <path d="M25 8c-1 10 0 20-1 29m0-14c-5-8-18-4-18 6-1 11 13 14 18 3" />
            <path d="M38 20c0 6-3 17 5 18 7 1 11-6 11-17m0 0-1 17" />
            <path d="m67 38 1-18m0 8c3-10 16-12 17-1l-1 11" />
            <path d="M98 29c6 1 13 0 20-2-1-10-17-11-20 0-4 12 11 16 19 9" />
          </g>
          <path className="brand-underline" d="M7 44c26-3 53 3 78-1 13-2 23-2 33-1" />
        </svg>
        <span className="brand-preview">preview</span>
      </span>
    </div>
  );
}
