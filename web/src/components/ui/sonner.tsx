import { useEffect, useState } from "react";
import { Toaster as SonnerToaster, type ToasterProps } from "sonner";

/** 跟随 <html class="dark"> 的明暗主题，供 toast 使用。 */
function useResolvedTheme(): "light" | "dark" {
  const [theme, setTheme] = useState<"light" | "dark">(() =>
    document.documentElement.classList.contains("dark") ? "dark" : "light",
  );

  useEffect(() => {
    const observer = new MutationObserver(() => {
      setTheme(document.documentElement.classList.contains("dark") ? "dark" : "light");
    });
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    return () => observer.disconnect();
  }, []);

  return theme;
}

/** 应用统一的 toast 出口，配色随主题切换。 */
export function Toaster(props: ToasterProps) {
  const theme = useResolvedTheme();

  return (
    <SonnerToaster
      theme={theme}
      position="top-center"
      richColors
      className="toaster group"
      toastOptions={{
        classNames: {
          toast: "text-sm",
          description: "text-xs",
        },
      }}
      {...props}
    />
  );
}
