import { useId } from "react";

/** Four matching arcs open left, up, down and right, with a planetary glint in E. */
export function Brand() {
  const id = useId();

  return (
    <div className="brand" role="img" aria-label="Dune · Preview">
      <svg className="brand-wordmark" viewBox="0 0 440 88" fill="none" aria-hidden="true" focusable="false">
        <defs>
          <linearGradient id={`${id}-stroke`} x1="0" y1="12" x2="0" y2="76" gradientUnits="userSpaceOnUse">
            <stop stopColor="#35433e" />
            <stop offset=".48" stopColor="#876747" />
            <stop offset="1" stopColor="#35433e" />
          </linearGradient>
          <linearGradient id={`${id}-light`} x1="322" y1="44" x2="438" y2="44" gradientUnits="userSpaceOnUse">
            <stop stopColor="#ac8049" stopOpacity="0" />
            <stop offset=".57" stopColor="#ac8049" stopOpacity=".8" />
            <stop offset="1" stopColor="#ac8049" stopOpacity="0" />
          </linearGradient>
          <radialGradient id={`${id}-halo`}>
            <stop stopColor="#d9b574" stopOpacity=".65" />
            <stop offset="1" stopColor="#d9b574" stopOpacity="0" />
          </radialGradient>
        </defs>
        <g className="brand-letters" stroke={`url(#${id}-stroke)`}>
          <path d="M12 12h32a32 32 0 0 1 0 64H12" />
          <path d="M124 12v32a32 32 0 0 0 64 0V12" />
          <path d="M236 76V44a32 32 0 0 1 64 0v32" />
          <path d="M412 12h-32a32 32 0 0 0 0 64h32" />
        </g>
        <ellipse cx="388" cy="44" rx="28" ry="10" fill={`url(#${id}-halo)`} />
        <path d="m322 44 66-1.5 50 1.5-50 1.5Z" fill={`url(#${id}-light)`} />
        <circle cx="388" cy="44" r="3.5" fill="var(--ink)" />
        <path d="M388 40.5a3.5 3.5 0 0 1 0 7" stroke="var(--cream)" strokeWidth="1.5" />
      </svg>
      <span className="brand-preview" aria-hidden="true">preview</span>
    </div>
  );
}
