import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const buttonVariants = cva("inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-lg border border-foreground/70 text-sm font-semibold transition-colors focus-visible:outline-2 focus-visible:outline-offset-3 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-45 [&_svg]:size-4", {
  variants: { variant: { default: "bg-primary text-primary-foreground shadow-[2px_2px_0_var(--ink)] hover:bg-primary/80", outline: "bg-card hover:bg-secondary", ghost: "border-transparent hover:bg-secondary", destructive: "bg-destructive text-white hover:bg-destructive/90" }, size: { default: "h-10 px-4", sm: "h-8 px-3 text-xs", icon: "size-9" } },
  defaultVariants: { variant: "default", size: "default" },
});
export function Button({ className, variant, size, asChild = false, ...props }: React.ComponentProps<"button"> & VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "button";
  return <Comp className={cn(buttonVariants({ variant, size, className }))} {...props} />;
}
