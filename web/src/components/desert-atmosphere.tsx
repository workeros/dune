const grains = Array.from({ length: 12 }, (_, index) => <i key={index} />);

/** Quiet, hand-drawn desert scenery that always stays behind the working UI. */
export function DesertAtmosphere() {
  return (
    <div className="desert-atmosphere" aria-hidden="true">
      <div className="desert-sand">{grains}</div>
      <svg className="desert-sky-lines" viewBox="0 0 1200 800" preserveAspectRatio="none" fill="none" focusable="false">
        <path d="M35 405c88-28 155-21 219-2 52 16 99 12 139-8m398-6c65-17 130-12 194 9M168 574c52-15 104-14 157 2 34 10 72 8 104-5m332 38c68-18 131-16 190 5" />
        <path className="desert-sky-line-light" d="M92 495c40-9 78-8 114 3m280-18c62-15 111-10 157 5m334 46c46-12 91-10 134 4M508 674c70-15 132-10 190 8" />
      </svg>
      <svg className="desert-sun" viewBox="0 0 100 100" fill="none" focusable="false">
        <circle cx="50" cy="50" r="21" />
        <g className="desert-sun-rays">
          <path d="M50 15V5m0 90V85M15 50H5m90 0H85M25 25l-8-8m66 66-8-8m0-50 8-8M17 83l8-8" />
        </g>
      </svg>
      <svg className="desert-worm" viewBox="0 0 380 170" fill="none" focusable="false">
        <path className="desert-worm-body" d="M15 151c38-39 77-57 116-49 29 6 42 31 72 29 29-2 40-38 66-62 25-22 62-24 87-5 14 11 22 27 24 46-26-17-49-18-69-5-27 18-42 49-76 58-39 10-66-17-96-23-40-8-76 5-108 27Z" />
        <path className="desert-worm-ring" d="M55 126c7 5 13 11 17 20m24-37c10 7 17 18 20 31m22-35c12 8 20 20 24 35m21-20c10 8 19 18 24 29m20-19c11 5 21 13 28 22m10-47c12 3 24 9 33 18" />
        <ellipse className="desert-worm-mouth" cx="322" cy="73" rx="49" ry="28" transform="rotate(15 322 73)" />
        <ellipse className="desert-worm-throat" cx="322" cy="73" rx="31" ry="16" transform="rotate(15 322 73)" />
        <path className="desert-worm-teeth" d="m281 54 17 14-14 8m30-25 3 19 13-16m13 7-13 13 21 2m-4 14-18-9 8 20m-25-7 5-15-18 10m-8-4 13-10-18-5" />
      </svg>
      <svg className="desert-horizon" viewBox="0 0 1200 300" preserveAspectRatio="none" fill="none" focusable="false">
        <path className="desert-dune-far" d="M0 76c126-49 255-50 384 2 111 45 213 31 308-13 128-59 262-40 508 67v168H0Z" />
        <path className="desert-dune-back" d="M0 147c112-53 225-63 343-10 92 42 181 35 267-3 109-48 207-43 310 6 93 44 183 34 280-21v181H0Z" />
        <path className="desert-dune-front" d="M0 220c149-38 265-15 355 40 109-80 224-87 346-30 127 59 260 44 499-47v117H0Z" />
        <path className="desert-dune-pencil desert-dune-pencil-far" d="M0 76c126-49 255-50 384 2 111 45 213 31 308-13 128-59 262-40 508 67" />
        <path className="desert-dune-pencil" d="M0 147c112-53 225-63 343-10 92 42 181 35 267-3 109-48 207-43 310 6 93 44 183 34 280-21M0 220c149-38 265-15 355 40 109-80 224-87 346-30 127 59 260 44 499-47" />
        <path className="desert-hatching" d="m118 182 31-18m-12 28 37-20m260 65 37-23m-16 35 43-27m255 14 34-22m-12 33 43-27m246-5 31-20" />
      </svg>
    </div>
  );
}
