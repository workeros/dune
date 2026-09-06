import type * as React from "react";
import { cn } from "@/lib/utils";
export function Input({ className, ...props }: React.ComponentProps<"input">) {
  return <input className={cn("h-10 w-full min-w-0 rounded-lg border border-foreground/40 bg-card px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50", className)} {...props} />;
}
export function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return <textarea className={cn("min-h-24 w-full rounded-lg border border-foreground/40 bg-card p-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring", className)} {...props} />;
}
